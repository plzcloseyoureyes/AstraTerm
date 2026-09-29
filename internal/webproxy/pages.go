package webproxy

import (
	"html/template"
	"net/http"
	"strings"
)

// bridgeConfig is embedded into the bridge script.
type bridgeConfig struct {
	ID      string   `json:"id"`
	Origins []string `json:"origins"` // AstraTerm UI origins allowed to talk to the bridge ("*" = any, error pages only)
	Prefix  string   `json:"prefix"`  // path-mode prefix
	Kind    string   `json:"kind"`
	Error   *pageErr `json:"error,omitempty"`
}

type pageErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

// bridgeJS reports navigation (path, title) of the proxied page to the AstraTerm tab that frames it and executes its
// toolbar commands (back, forward, reload, navigate). It only talks to the configured AstraTerm origins and only acts in
// the frame AstraTerm embeds directly (not in the application's own nested frames, not in a top-level window).
const bridgeJS = `(function(){
var C=__CONFIG__;
if(window.__astraterm_bridge)return;window.__astraterm_bridge=1;
var P=window.parent;if(!P||P===window)return;
try{if(P.location.href){return}}catch(e){}
function post(m){m.source='astraterm-webproxy';m.proxyId=C.id;for(var i=0;i<C.origins.length;i++){try{P.postMessage(m,C.origins[i])}catch(e){}}}
function rel(){var p=location.pathname+location.search+location.hash;if(C.prefix&&p.indexOf(C.prefix)===0){p=p.slice(C.prefix.length)||'/'}return p}
var last='';function report(){var k=rel()+'\n'+document.title;if(k===last)return;last=k;post({type:'location',path:rel(),title:document.title||''})}
var t=0;function soon(){clearTimeout(t);t=setTimeout(report,50)}
['pushState','replaceState'].forEach(function(n){var o=history[n];if(typeof o!=='function')return;history[n]=function(){var r=o.apply(this,arguments);soon();return r}});
addEventListener('popstate',soon);addEventListener('hashchange',soon);
addEventListener('pagehide',function(){post({type:'unload'})});
addEventListener('message',function(e){if(e.source!==P)return;if(C.origins.indexOf('*')<0&&C.origins.indexOf(e.origin)<0)return;var d=e.data;if(!d||d.source!=='astraterm-webproxy'||!d.cmd)return;
switch(d.cmd){case'back':history.back();break;case'forward':history.forward();break;case'reload':location.reload();break;
case'navigate':if(typeof d.path==='string'&&d.path.charAt(0)==='/'&&d.path.charAt(1)!=='/'){location.href=C.prefix+d.path}break;
case'ping':last='';report();break}});
function ready(){if(C.error){post({type:'error',error:C.error,path:rel(),title:document.title||''})}else{report()}
try{var h=document.querySelector('head')||document.documentElement;new MutationObserver(soon).observe(h,{subtree:true,childList:true,characterData:true})}catch(e){}}
post({type:'hello'});
if(document.readyState==='loading'){document.addEventListener('DOMContentLoaded',ready)}else{ready()}
})();`

// bridgeScript returns the <script> element injected into proxied HTML documents.
func bridgeScript(cfg bridgeConfig, nonce string) string {
	if cfg.Origins == nil {
		cfg.Origins = []string{}
	}
	js := strings.Replace(bridgeJS, "__CONFIG__", jsonForScript(cfg), 1)
	attr := ""
	if nonce != "" {
		attr = ` nonce="` + nonce + `"`
	}
	return "<script" + attr + ">" + js + "</script>"
}

// jsonForScript marshals v for embedding inside a <script> element.
func jsonForScript(v any) string {
	s := marshalJSON(v)
	// encoding/json already escapes these; the replacer keeps the guarantee independent of the marshaller.
	r := strings.NewReplacer("<", `\u003c`, ">", `\u003e`, "&", `\u0026`, "\u2028", `\u2028`, "\u2029", `\u2029`)
	return r.Replace(s)
}

// page is a AstraTerm-rendered page on a proxy origin (errors, closed proxies, authentication).
type page struct {
	Code    string
	Title   string
	Message string
	Target  string
	Via     string
	ProxyID string
	Prefix  string
	Origins []string // bridge targets
	Frame   []string // frame-ancestors (nil = any: the page reveals nothing)
	Sandbox bool     // path mode: opaque origin
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light dark"><title>{{.Title}}</title>
<style nonce="{{.Nonce}}">
:root{--bg:#fbfbfc;--fg:#1c2024;--muted:#60646c;--line:#e0e1e6;--accent:#3e63dd;--card:#fff}
@media (prefers-color-scheme:dark){:root{--bg:#111113;--fg:#edeef0;--muted:#a0a3aa;--line:#2e3035;--accent:#8da4ef;--card:#18191b}}
*{box-sizing:border-box}html,body{height:100%;margin:0}
body{display:grid;place-items:center;background:var(--bg);color:var(--fg);font:13px/1.5 Inter,system-ui,-apple-system,"Segoe UI",sans-serif;padding:16px}
main{max-width:34rem;width:100%;background:var(--card);border:1px solid var(--line);border-radius:10px;padding:20px 22px}
h1{font-size:15px;margin:0 0 6px;font-weight:600}p{margin:0 0 10px;color:var(--muted)}
dl{display:grid;grid-template-columns:auto 1fr;gap:2px 12px;margin:12px 0 14px;font-size:12px}dt{color:var(--muted)}dd{margin:0;word-break:break-all;font-family:ui-monospace,Menlo,monospace}
button{font:inherit;border:1px solid var(--line);background:transparent;color:var(--fg);border-radius:6px;padding:4px 12px;cursor:pointer}
button:hover{border-color:var(--accent)}button:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
.code{font-size:11px;color:var(--muted);float:right}
</style></head>
<body><main role="alert"><span class="code">{{.Status}}</span><h1>{{.Title}}</h1><p>{{.Message}}</p>
{{if .Target}}<dl><dt>Address</dt><dd>{{.Target}}</dd>{{if .Via}}<dt>Through</dt><dd>{{.Via}}</dd>{{end}}</dl>{{end}}
{{if .Retry}}<button type="button" id="retry" autofocus>Try again</button>{{end}}
</main>
<script nonce="{{.Nonce}}">{{.Bridge}}
(function(){var b=document.getElementById('retry');if(b)b.addEventListener('click',function(){location.reload()})})();</script>
</body></html>`))

// writePage renders p with a strict CSP of its own.
func writePage(w http.ResponseWriter, r *http.Request, status int, p page) {
	nonce := randomToken(18)
	fa := "*"
	if p.Frame != nil {
		fa = frameAncestors(p.Frame)
	}
	csp := "default-src 'none'; style-src 'nonce-" + nonce + "'; script-src 'nonce-" + nonce + "'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors " + fa
	if p.Sandbox {
		csp = "sandbox allow-scripts; " + csp
	}
	h := w.Header()
	clearAstraTermHeaders(h, false)
	h.Set("Content-Security-Policy", csp)
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	cfg := bridgeConfig{ID: p.ProxyID, Origins: p.Origins, Prefix: p.Prefix, Error: &pageErr{Code: p.Code, Message: p.Message, Status: status}}
	if cfg.Origins == nil {
		cfg.Origins = []string{}
	}
	js := strings.Replace(bridgeJS, "__CONFIG__", jsonForScript(cfg), 1)
	_ = pageTmpl.Execute(w, struct {
		page
		Status int
		Nonce  string
		Retry  bool
		Bridge template.JS
	}{p, status, nonce, p.Code != "gone" && p.Code != "auth", template.JS(js)})
}
