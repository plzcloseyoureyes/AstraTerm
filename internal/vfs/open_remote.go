package vfs

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/studio-b12/gowebdav"

	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/netguard"
)

// Connection options read by the non-SSH drivers (SPEC §5.3 + §9 files-backend):
//   ftp:    ftpTls none|explicit|implicit, insecureTls, ftpEpsv (true), ftpMlsd (true), ftpTrustPasvIP (false),
//           ftpMaxConnections (4), initialPath; secret password (anonymous when no username)
//   s3:     endpoint (URL; or host/port + https), region, bucket, pathStyle, accessKeyId, profile, insecureTls,
//           initialPath; secrets secretAccessKey, sessionToken
//   webdav: url (full base URL) or host/port + https (default true) + basePath, insecureTls, initialPath; secret password
//   smb:    share, domain, initialPath; secret password

func (r *Registry) openFTP(ctx context.Context, user *model.User, conn *model.Connection, secrets map[string]string) (*Handle, error) {
	o := conn.Options
	tlsMode := strings.ToLower(o.String("ftpTls", "none"))
	switch tlsMode {
	case "none", "explicit", "implicit":
	default:
		return nil, httpx.BadRequest("ftpTls must be none, explicit or implicit")
	}
	username, pass := conn.Username, secrets[model.SecretPassword]
	if username != "" && !strings.EqualFold(username, "anonymous") && !strings.EqualFold(username, "ftp") && pass == "" {
		pw, _, err := r.askSecret(ctx, user, conn, model.SecretPassword, "FTP password", "Password")
		if err != nil {
			return nil, err
		}
		pass = pw
	}
	dial, closeDial, err := r.dialer(ctx, user, conn, secrets)
	if err != nil {
		return nil, err
	}
	f, err := newFTPFS(ctx, ftpConfig{
		Host: conn.Host, Port: conn.Port, User: username, Pass: pass, TLS: tlsMode,
		Insecure: o.Bool("insecureTls"), EPSV: o.Bool("ftpEpsv", true), MLSD: o.Bool("ftpMlsd", true),
		TrustPasvIP: o.Bool("ftpTrustPasvIP", false), MaxConns: o.Int("ftpMaxConnections", 4),
	}, dial)
	if err != nil {
		closeDial()
		return nil, err
	}
	f.closeFn = closeDial
	h := &Handle{Kind: "ftp", Driver: "ftp", FS: f, Protocol: string(conn.Protocol), Host: conn.Host, Username: username, Port: conn.Port}
	h.UserHome = f.home
	h.Home = r.startDir(ctx, f, conn, f.home)
	h.Label = connLabel(conn)
	h.Capabilities = capsFor(f, "ftp", false)
	return h, nil
}

// httpTransport builds an HTTP transport that dials through the connection's proxy / jump chain.
func httpTransport(dial func(ctx context.Context, addr string) (net.Conn, error), insecure bool) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dial(ctx, addr)
		},
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: insecure, MinVersion: tls.VersionTLS12}, //nolint:gosec // per-connection option
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
		ExpectContinueTimeout: time.Second,
	}
}

func (r *Registry) openS3(ctx context.Context, user *model.User, conn *model.Connection, secrets map[string]string) (*Handle, error) {
	o := conn.Options
	region := o.String("region", "us-east-1")
	endpoint := strings.TrimSpace(o.String("endpoint", ""))
	if endpoint == "" && conn.Host != "" {
		scheme := "https"
		if !o.Bool("https", true) {
			scheme = "http"
		}
		endpoint = scheme + "://" + hostPort(conn.Host, conn.Port, 443)
	}
	if endpoint != "" && !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	var endpointURL *url.URL
	if endpoint != "" {
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, httpx.BadRequest("invalid S3 endpoint URL")
		}
		endpointURL = u
	}
	bucket := strings.Trim(o.String("bucket", ""), "/")
	ak := strings.TrimSpace(o.String("accessKeyId", ""))
	var cfg aws.Config
	if ak != "" {
		sk := secrets[model.SecretSecretAccessKey]
		if sk == "" {
			pw, _, err := r.askSecret(ctx, user, conn, model.SecretSecretAccessKey, "S3 secret key", "Secret access key")
			if err != nil {
				return nil, err
			}
			sk = pw
		}
		cfg = aws.Config{Region: region, Credentials: credentials.NewStaticCredentialsProvider(ak, sk, secrets["sessionToken"])}
	} else {
		desktop := r.d != nil && r.d.Cfg != nil && r.d.Cfg.IsDesktop()
		if !desktop && !user.IsAdmin() {
			return nil, httpx.BadRequest("an access key ID is required (host AWS profiles are only used in desktop mode or by administrators)")
		}
		opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
		if p := strings.TrimSpace(o.String("profile", "")); p != "" {
			opts = append(opts, awsconfig.WithSharedConfigProfile(p))
		}
		var err error
		if cfg, err = awsconfig.LoadDefaultConfig(ctx, opts...); err != nil {
			return nil, fmt.Errorf("load AWS configuration: %w", err)
		}
	}
	var closeDial func()
	var transport http.RoundTripper
	// Restricted users (netguard) always get NexTerm's own transport (never the environment's proxies).
	if needsDialer(conn) || endpointURL != nil || netguard.ForUser(r.d, user) != nil {
		dconn := conn.Clone()
		if endpointURL != nil {
			dconn.Host = endpointURL.Hostname()
		}
		dial, cd, err := r.dialer(ctx, user, dconn, secrets)
		if err != nil {
			return nil, err
		}
		closeDial = cd
		transport = httpTransport(dial, o.Bool("insecureTls"))
	}
	client := s3.NewFromConfig(cfg, func(so *s3.Options) {
		if endpointURL != nil {
			so.BaseEndpoint = aws.String(strings.TrimSuffix(endpointURL.String(), "/"))
		}
		so.UsePathStyle = o.Bool("pathStyle")
		so.Region = region
		so.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		so.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		if transport != nil {
			so.HTTPClient = &http.Client{Transport: transport}
		}
	})
	f := newS3FS(client, bucket, "/", closeDial)
	// Validate credentials / bucket now so errors surface at open time.
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	_, err := f.List(vctx, "/")
	cancel()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		f.Close()
		return nil, err
	}
	h := &Handle{Kind: "s3", Driver: "s3", FS: f, Protocol: string(conn.Protocol), Host: conn.Host, Port: conn.Port}
	h.UserHome = "/"
	h.Home = r.startDir(ctx, f, conn, "/")
	f.home = h.Home
	h.Label = connLabel(conn)
	if h.Label == "s3" && bucket != "" {
		h.Label = "s3://" + bucket
	}
	h.Capabilities = capsFor(f, "s3", false)
	return h, nil
}

// needsDialer reports whether the connection routes through a proxy or SSH gateway.
func needsDialer(conn *model.Connection) bool {
	o := conn.Options
	if o.String("sshTunnelVia", "") != "" || len(o.Strings("jumpHosts")) > 0 || o.String("proxyCommand", "") != "" {
		return true
	}
	if m := o.Map("proxy"); m != nil {
		if t, _ := m["type"].(string); t != "" && t != "none" {
			return true
		}
	}
	return false
}

func (r *Registry) openWebDAV(ctx context.Context, user *model.User, conn *model.Connection, secrets map[string]string) (*Handle, error) {
	o := conn.Options
	base := strings.TrimSpace(o.String("url", ""))
	if base == "" {
		if conn.Host == "" {
			return nil, httpx.BadRequest("a WebDAV URL or host is required")
		}
		scheme := "https"
		def := 443
		if strings.EqualFold(string(conn.Protocol), "webdav") && !o.Bool("https", conn.Port != 80) {
			scheme, def = "http", 80
		}
		bp := "/" + strings.Trim(o.String("basePath", ""), "/")
		base = scheme + "://" + hostPort(conn.Host, conn.Port, def) + bp
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, httpx.BadRequest("invalid WebDAV URL")
	}
	username, pass := conn.Username, secrets[model.SecretPassword]
	if username != "" && pass == "" {
		pw, _, err := r.askSecret(ctx, user, conn, model.SecretPassword, "WebDAV password", "Password")
		if err != nil {
			return nil, err
		}
		pass = pw
	}
	dconn := conn.Clone()
	dconn.Host = u.Hostname()
	dial, closeDial, err := r.dialer(ctx, user, dconn, secrets)
	if err != nil {
		return nil, err
	}
	c := gowebdav.NewClient(base, username, pass)
	c.SetTransport(&davRedirectTransport{base: httpTransport(dial, o.Bool("insecureTls"))})
	if err := c.Connect(); err != nil {
		closeDial()
		err = davErr(err, "/")
		if errors.Is(err, fs.ErrPermission) {
			return nil, fmt.Errorf("WebDAV login failed: %w", fs.ErrPermission)
		}
		return nil, fmt.Errorf("WebDAV connection failed: %w", err)
	}
	f := newWebDAVFS(c, r.tmpDir(), "/", closeDial)
	h := &Handle{Kind: "webdav", Driver: "webdav", FS: f, Protocol: string(conn.Protocol), Host: u.Hostname(), Username: username, Port: conn.Port}
	h.UserHome = "/"
	h.Home = r.startDir(ctx, f, conn, "/")
	h.Label = connLabel(conn)
	h.Capabilities = capsFor(f, "webdav", false)
	return h, nil
}

func (r *Registry) openSMB(ctx context.Context, user *model.User, conn *model.Connection, secrets map[string]string) (*Handle, error) {
	o := conn.Options
	if conn.Host == "" {
		return nil, httpx.BadRequest("host is required")
	}
	username, pass := conn.Username, secrets[model.SecretPassword]
	if username == "" {
		username = "guest"
	} else if pass == "" {
		pw, _, err := r.askSecret(ctx, user, conn, model.SecretPassword, "SMB password", "Password")
		if err != nil {
			return nil, err
		}
		pass = pw
	}
	dial, closeDial, err := r.dialer(ctx, user, conn, secrets)
	if err != nil {
		return nil, err
	}
	addr := hostPort(conn.Host, conn.Port, 445)
	f, err := newSMBFS(ctx, conn.Host, o.String("share", ""), username, pass, o.String("domain", ""), "/",
		func(ctx context.Context) (net.Conn, error) { return dial(ctx, addr) })
	if err != nil {
		closeDial()
		return nil, err
	}
	f.closeFn = closeDial
	h := &Handle{Kind: "smb", Driver: "smb", FS: f, Protocol: string(conn.Protocol), Host: conn.Host, Username: username,
		Port: conn.Port}
	h.UserHome = "/"
	h.Home = r.startDir(ctx, f, conn, "/")
	f.home = h.Home
	h.Label = connLabel(conn)
	h.Capabilities = capsFor(f, "smb", false)
	return h, nil
}

// davRedirectTransport follows the "add a trailing slash" redirect servers send for collections (Apache mod_dav) by
// replaying the same WebDAV method. Go's http.Client would otherwise turn a redirected PROPFIND / DELETE / MOVE into
// a GET (silently "succeeding" without doing anything).
type davRedirectTransport struct{ base http.RoundTripper }

func (t *davRedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || req.Method == http.MethodGet || req.Method == http.MethodHead {
		return resp, err
	}
	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return resp, err
	}
	loc, lerr := req.URL.Parse(resp.Header.Get("Location"))
	if lerr != nil || loc.Host != req.URL.Host || loc.EscapedPath() != req.URL.EscapedPath()+"/" {
		return resp, err
	}
	next := req.Clone(req.Context())
	next.URL = loc
	next.Host = ""
	if req.Body != nil && req.Body != http.NoBody {
		if req.GetBody == nil {
			return resp, err
		}
		body, berr := req.GetBody()
		if berr != nil {
			return resp, err
		}
		next.Body = body
	}
	resp.Body.Close()
	return t.base.RoundTrip(next)
}
