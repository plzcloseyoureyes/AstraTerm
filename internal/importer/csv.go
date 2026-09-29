package importer

import (
	"encoding/csv"
	"strings"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// CSV importers: Termius export (fixed header) and a generic CSV with header auto-detection plus an optional caller
// column mapping. Passwords present in a CSV column are dropped with a warning (never stored from an import file).

// parseTermiusCSV imports a Termius CSV export: Groups,Label,Tags,Hostname/IP,Protocol,Port,Username,Password.
func parseTermiusCSV(content []byte) (*parsed, error) {
	return parseCSVWith(content, fmtTermiusCSV, previewOptions{})
}

// parseGenericCSV imports an arbitrary CSV, mapping columns by the caller's csvMapping or a set of common aliases.
func parseGenericCSV(content []byte, opts previewOptions) (*parsed, error) {
	return parseCSVWith(content, fmtCSV, opts)
}

func parseCSVWith(content []byte, format string, opts previewOptions) (*parsed, error) {
	text := decodeMaybeCP1252(content)
	r := csv.NewReader(strings.NewReader(text))
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	r.LazyQuotes = true
	if d := opts.CSVDelim; d != "" {
		r.Comma = delimRune(d)
	} else if d := sniffDelimiter(firstNonEmptyLine(text)); d != 0 {
		r.Comma = d
	}
	rows, err := r.ReadAll()
	if err != nil {
		return nil, badRequest("invalid CSV: " + err.Error())
	}
	// Skip leading blank rows.
	for len(rows) > 0 && isBlankRow(rows[0]) {
		rows = rows[1:]
	}
	if len(rows) == 0 {
		return nil, badRequest("the CSV is empty")
	}
	header := rows[0]
	colIdx := map[string]int{} // field name → column index
	for i, h := range header {
		field := mapHeader(strings.TrimSpace(h), opts.CSVMapping)
		if field != "" {
			if _, ok := colIdx[field]; !ok {
				colIdx[field] = i
			}
		}
	}
	if _, ok := colIdx["host"]; !ok {
		return nil, badRequest("no host/address column found in the CSV header (map it explicitly)")
	}
	b := newBuilder(format)
	droppedPassword := false
	get := func(row []string, field string) string {
		if i, ok := colIdx[field]; ok && i < len(row) {
			return strings.TrimSpace(row[i])
		}
		return ""
	}
	for _, row := range rows[1:] {
		if isBlankRow(row) {
			continue
		}
		host, user, port := get(row, "host"), get(row, "username"), atoiSafe(get(row, "port"), 0)
		proto := parseProtocolName(get(row, "protocol"))
		// A Termius URI-style host ("ssh://user@host:port") is split apart; an inline password is dropped.
		if u, ok := parseURIHost(host); ok {
			if u.proto != "" {
				proto = u.proto
			}
			host = u.host
			if u.hadPassword {
				droppedPassword = true
			}
			if user == "" {
				user = u.user
			}
			if port == 0 {
				port = u.port
			}
		}
		if host == "" {
			continue
		}
		if proto == "" {
			proto = model.ProtoSSH
		}
		folderID := ""
		if g := get(row, "folder"); g != "" {
			folderID = b.folderPath("", strings.FieldsFunc(g, func(r rune) bool { return r == '/' || r == '\\' })...)
		}
		c := model.Connection{
			Name:     cleanName(get(row, "name"), host),
			Protocol: proto,
			Host:     host,
			Port:     clampPort(port, proto),
			Username: user,
			Notes:    get(row, "notes"),
			Tags:     cleanTags(splitList(get(row, "tags"), ", ;|")),
			Options:  model.Options{},
		}
		b.conn(folderID, c)
		if get(row, "password") != "" {
			droppedPassword = true
		}
	}
	if len(b.p.conns) == 0 {
		return nil, badRequest("no rows with a host were found in the CSV")
	}
	if droppedPassword {
		b.warn("Passwords in the CSV were not imported for safety. Re-enter them after import.")
	}
	return b.finish()
}

// mapHeader maps a CSV header cell to a normalized field name, honouring an explicit user mapping first.
func mapHeader(h string, mapping map[string]string) string {
	if mapping != nil {
		if v, ok := mapping[h]; ok {
			return strings.ToLower(strings.TrimSpace(v))
		}
	}
	switch strings.ToLower(strings.TrimSpace(h)) {
	case "name", "label", "title", "session", "session name", "alias", "nickname":
		return "name"
	case "host", "hostname", "hostname/ip", "address", "ip", "ip address", "server", "host/ip":
		return "host"
	case "port":
		return "port"
	case "username", "user", "login", "user name":
		return "username"
	case "protocol", "type", "proto", "scheme":
		return "protocol"
	case "group", "groups", "folder", "path", "category", "tag group":
		return "folder"
	case "tags", "labels":
		return "tags"
	case "notes", "note", "description", "comment", "comments", "descr":
		return "notes"
	case "password", "pass", "pwd", "secret":
		return "password"
	}
	return ""
}

// parseProtocolName maps a free-form protocol string to a AstraTerm protocol ("" when unknown).
func parseProtocolName(s string) model.Protocol {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "ssh", "ssh2", "sshv2", "secure shell":
		return model.ProtoSSH
	case "telnet", "telnet/ssl", "telnets":
		return model.ProtoTelnet
	case "rlogin", "rsh":
		return model.ProtoRlogin
	case "raw", "rawtcp", "tcp":
		return model.ProtoRaw
	case "serial", "com":
		return model.ProtoSerial
	case "mosh":
		return model.ProtoMosh
	case "sftp":
		return model.ProtoSFTP
	case "ftp", "ftps":
		return model.ProtoFTP
	case "s3":
		return model.ProtoS3
	case "vnc":
		return model.ProtoVNC
	case "rdp", "ms-rdp", "remote desktop":
		return model.ProtoRDP
	case "winrm":
		return model.ProtoWinRM
	case "ipmi":
		return model.ProtoIPMI
	case "docker":
		return model.ProtoDocker
	case "kube", "kubernetes", "k8s":
		return model.ProtoKube
	}
	return ""
}

type uriHost struct {
	proto       model.Protocol
	user        string
	host        string
	port        int
	hadPassword bool
}

// parseURIHost splits a "scheme://[user[:password]@]host[:port][/path]" address; ok is false when s is a plain host.
// A password in the user info is never kept (hadPassword reports it).
func parseURIHost(s string) (uriHost, bool) {
	i := strings.Index(s, "://")
	if i < 0 {
		return uriHost{}, false
	}
	var u uriHost
	u.proto = parseProtocolName(s[:i])
	rest := s[i+3:]
	if slash := strings.IndexAny(rest, "/?#"); slash >= 0 {
		rest = rest[:slash]
	}
	if at := strings.LastIndexByte(rest, '@'); at >= 0 {
		u.user = rest[:at]
		rest = rest[at+1:]
		if c := strings.IndexByte(u.user, ':'); c >= 0 {
			u.user, u.hadPassword = u.user[:c], true
		}
	}
	u.host, u.port = splitHostPortDefault(rest)
	return u, u.host != ""
}

func isBlankRow(row []string) bool {
	for _, c := range row {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}

func delimRune(s string) rune {
	switch s {
	case "\\t", "\t", "tab":
		return '\t'
	case ";":
		return ';'
	case "|":
		return '|'
	default:
		return ','
	}
}
