// Command notices generates THIRD_PARTY_NOTICES.md: every Go module compiled into the astraterm binary (for every
// release platform) and every npm package bundled into the embedded web UI, with its license and the full license
// texts shipped by the component.
//
//	go run ./scripts/release/notices            # rewrite THIRD_PARTY_NOTICES.md
//	go run ./scripts/release/notices -check     # exit 1 when the file is out of date (CI)
//
// Inputs: the Go module cache (modules are downloaded when missing) and web/node_modules (run `npm ci` in web/
// first). The output is deterministic: sorted, no time stamps, no machine-specific paths.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// platforms mirrors the release matrix (Makefile PLATFORMS, scripts/release/dist): platform-specific dependencies
// (ConPTY, go-winio, …) are only found by listing the packages for each of them.
var platforms = []string{
	"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64", "linux/arm/7", "windows/amd64", "windows/arm64", "freebsd/amd64",
}

// bundledBuildTools are devDependencies whose own code ends up in the bundle (runtime helpers, generated CSS).
var bundledBuildTools = map[string]string{
	"vite":        "module preload helper emitted into the bundle",
	"rolldown":    "bundler runtime helpers emitted into the bundle",
	"tailwindcss": "preflight and utility CSS generated into the stylesheet",
}

// Vendored or patched third-party code inside this repository (kept in sync by hand; see the notes in the files).
const repoNotes = `### Modified third-party code in this repository

- ` + "`web/src/features/editor/monaco/vendor/monaco-vim.js`" + ` is a modified copy of monaco-vim 0.4.4 (MIT;
  its Vim keymap derives from CodeMirror, MIT). Changes: import specifiers and one constructor argument (see the file
  header). The MIT notices of both are reproduced below under ` + "`monaco-vim`" + `.
- ` + "`web/src/features/termtransfer/engine/zmodem.ts`" + ` replaces a few internal methods of zmodem.js 0.1.10
  (Apache-2.0) at run time to work around protocol-edge bugs. The zmodem.js files themselves are bundled unmodified;
  the changes live in AstraTerm's own source file, which states what it changes (Apache-2.0 section 4(b)).
- trzsz (MIT) is used through its public API only; no files are modified.
`

// copyleft license identifiers that carry obligations beyond keeping the notice.
var copyleft = []string{"MPL-2.0", "LGPL", "GPL", "AGPL", "EPL", "CDDL", "EUPL", "OFL-1.1", "CC-BY-SA"}

type component struct {
	Kind      string // "go" or "npm"
	Name      string
	Version   string
	License   string // SPDX expression (declared, or detected from the texts)
	Dir       string
	Note      string
	Platforms []string // Go only: nil = every platform
	Texts     []licenseText
}

type licenseText struct {
	File string
	Text string
}

func main() {
	out := flag.String("o", "THIRD_PARTY_NOTICES.md", "output file")
	check := flag.Bool("check", false, "compare with the existing file instead of writing it")
	web := flag.String("web", "web", "frontend directory (package-lock.json, node_modules)")
	flag.Parse()

	goComps, err := goComponents()
	if err != nil {
		fail(err)
	}
	npmComps, err := npmComponents(*web)
	if err != nil {
		fail(err)
	}
	doc := render(goComps, npmComps)
	if *check {
		old, err := os.ReadFile(*out)
		if err != nil || !bytes.Equal(old, doc) {
			fmt.Fprintf(os.Stderr, "notices: %s is out of date; run `make notices` and commit the result\n", *out)
			os.Exit(1)
		}
		fmt.Printf("notices: %s is up to date (%d Go modules, %d npm packages)\n", *out, len(goComps), len(npmComps))
		return
	}
	if err := os.WriteFile(*out, doc, 0o644); err != nil {
		fail(err)
	}
	fmt.Printf("notices: wrote %s (%d Go modules, %d npm packages)\n", *out, len(goComps), len(npmComps))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "notices:", err)
	os.Exit(1)
}

// ---- Go ----------------------------------------------------------------------------------------------------------

type goModule struct {
	Path    string
	Version string
	Dir     string
	Main    bool
	Replace *goModule
}

func goComponents() ([]*component, error) {
	seen := map[string][]string{} // module path → platforms
	versions := map[string]string{}
	for _, p := range platforms {
		goos, goarch, _ := strings.Cut(p, "/")
		goarch, goarm, _ := strings.Cut(goarch, "/") // linux/arm/7: GOARCH=arm GOARM=7
		cmd := exec.Command("go", "list", "-deps", "-f",
			`{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}}{{end}}{{end}}`, "./cmd/astraterm")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch, "GOARM="+goarm, "GOFLAGS=-mod=readonly")
		cmd.Stderr = os.Stderr
		b, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go list (%s): %w", p, err)
		}
		for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
			mod, ver, ok := strings.Cut(line, " ")
			if !ok || slices.Contains(seen[mod], p) {
				continue
			}
			seen[mod] = append(seen[mod], p)
			versions[mod] = ver
		}
	}
	mods := map[string]goModule{}
	b, err := exec.Command("go", "list", "-m", "-json", "all").Output()
	if err != nil {
		return nil, fmt.Errorf("go list -m: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	for dec.More() {
		var m goModule
		if err := dec.Decode(&m); err != nil {
			return nil, err
		}
		if m.Replace != nil {
			m.Dir = m.Replace.Dir
		}
		mods[m.Path] = m
	}

	var comps []*component
	for mod, plats := range seen {
		m := mods[mod]
		if m.Dir == "" { // not in the module cache yet
			out, err := exec.Command("go", "mod", "download", "-json", mod+"@"+versions[mod]).Output()
			if err != nil {
				return nil, fmt.Errorf("go mod download %s: %w", mod, err)
			}
			var dl struct{ Dir string }
			if err := json.Unmarshal(out, &dl); err != nil {
				return nil, err
			}
			m.Dir = dl.Dir
		}
		c := &component{Kind: "go", Name: mod, Version: versions[mod], Dir: m.Dir}
		if len(plats) < len(platforms) {
			c.Platforms = plats
			sort.Strings(c.Platforms)
		}
		if c.Texts, err = readLicenseFiles(m.Dir); err != nil {
			return nil, err
		}
		c.License = detect(c.Texts)
		comps = append(comps, c)
	}

	// The Go standard library and runtime are compiled into every binary.
	goroot, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		return nil, err
	}
	gover, _ := exec.Command("go", "env", "GOVERSION").Output()
	std := &component{Kind: "go", Name: "Go standard library and runtime", Version: strings.TrimSpace(string(gover)),
		Dir: strings.TrimSpace(string(goroot))}
	if std.Texts, err = readLicenseFiles(std.Dir); err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(std.Texts, func(t licenseText) bool { return t.File == "LICENSE" }) {
		// Some packaged toolchains (Homebrew) keep LICENSE one level above GOROOT.
		parent, err := readLicenseFiles(filepath.Dir(std.Dir))
		if err != nil {
			return nil, err
		}
		std.Texts = append(std.Texts, parent...)
		sort.Slice(std.Texts, func(i, j int) bool { return std.Texts[i].File < std.Texts[j].File })
	}
	std.Texts = slices.DeleteFunc(std.Texts, func(t licenseText) bool { return t.File != "LICENSE" && t.File != "PATENTS" })
	std.License = detect(std.Texts)
	comps = append(comps, std)
	sort.Slice(comps, func(i, j int) bool { return comps[i].Name < comps[j].Name })
	return comps, nil
}

// ---- npm ---------------------------------------------------------------------------------------------------------

type lockEntry struct {
	Version string `json:"version"`
	License any    `json:"license"`
	Dev     bool   `json:"dev"`
}

func npmComponents(web string) ([]*component, error) {
	b, err := os.ReadFile(filepath.Join(web, "package-lock.json"))
	if err != nil {
		return nil, err
	}
	var lock struct {
		LockfileVersion int                  `json:"lockfileVersion"`
		Packages        map[string]lockEntry `json:"packages"`
	}
	if err := json.Unmarshal(b, &lock); err != nil {
		return nil, err
	}
	if lock.LockfileVersion < 2 {
		return nil, errors.New("package-lock.json v2+ required")
	}
	if _, err := os.Stat(filepath.Join(web, "node_modules")); err != nil {
		return nil, fmt.Errorf("%s/node_modules missing: run `npm ci` in %s first", web, web)
	}
	var comps []*component
	for key, e := range lock.Packages {
		if key == "" || !strings.HasPrefix(key, "node_modules/") {
			continue
		}
		name := key[strings.LastIndex(key, "node_modules/")+len("node_modules/"):]
		note, tool := bundledBuildTools[name]
		if e.Dev && !(tool && !strings.Contains(strings.TrimPrefix(key, "node_modules/"), "node_modules/")) {
			continue
		}
		dir := filepath.Join(web, filepath.FromSlash(key))
		c := &component{Kind: "npm", Name: name, Version: e.Version, Dir: dir, License: spdx(e.License)}
		if tool {
			c.Note = "build tool: " + note
		}
		if _, err := os.Stat(dir); err != nil {
			c.Note = strings.TrimPrefix(c.Note+"; not installed on the generating machine (optional dependency)", "; ")
		} else if c.Texts, err = readLicenseFiles(dir); err != nil {
			return nil, err
		}
		if c.License == "" {
			c.License = detect(c.Texts)
		}
		comps = append(comps, c)
	}
	sort.Slice(comps, func(i, j int) bool {
		if comps[i].Name != comps[j].Name {
			return comps[i].Name < comps[j].Name
		}
		return comps[i].Version < comps[j].Version
	})
	return comps, nil
}

func spdx(v any) string {
	switch l := v.(type) {
	case string:
		l = strings.TrimSpace(l)
		if l == "Apache 2.0" {
			return "Apache-2.0"
		}
		return l
	case map[string]any:
		if t, ok := l["type"].(string); ok {
			return t
		}
	}
	return ""
}

// ---- license files -----------------------------------------------------------------------------------------------

var licenseFileRE = regexp.MustCompile(`(?i)^(licen[cs]e|copying|notice|copyright|unlicense|patents|thirdpartynotices|third[-_]party[-_]notices)([-._].*)?$`)

func readLicenseFiles(dir string) ([]licenseText, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var texts []licenseText
	for _, e := range entries {
		if e.IsDir() || !licenseFileRE.MatchString(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		t := strings.TrimSpace(strings.ReplaceAll(string(b), "\r\n", "\n"))
		if t != "" {
			texts = append(texts, licenseText{File: e.Name(), Text: t})
		}
	}
	sort.Slice(texts, func(i, j int) bool { return texts[i].File < texts[j].File })
	return texts, nil
}

// detect names the licenses found in the texts (a heuristic; declared SPDX identifiers take precedence).
func detect(texts []licenseText) string {
	var ids []string
	add := func(id string) {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	for _, t := range texts {
		s := strings.Join(strings.Fields(t.Text), " ")
		switch {
		case strings.Contains(s, "Mozilla Public License Version 2.0") || strings.Contains(s, "Mozilla Public License, version 2.0"):
			add("MPL-2.0")
		case strings.Contains(s, "GNU LESSER GENERAL PUBLIC LICENSE"):
			add("LGPL")
		case strings.Contains(s, "GNU AFFERO GENERAL PUBLIC LICENSE"):
			add("AGPL")
		case strings.Contains(s, "GNU GENERAL PUBLIC LICENSE"):
			add("GPL")
		case strings.Contains(s, "Apache License") && strings.Contains(s, "Version 2.0"):
			add("Apache-2.0")
		case strings.Contains(s, "Permission is hereby granted, free of charge"):
			add("MIT")
		case strings.Contains(s, "Redistribution and use in source and binary forms"):
			if strings.Contains(s, "Neither the name") || strings.Contains(s, "names of its contributors") ||
				strings.Contains(s, "name of the copyright holder") {
				add("BSD-3-Clause")
			} else {
				add("BSD-2-Clause")
			}
		case strings.Contains(s, "Permission to use, copy, modify, and/or distribute this software for any purpose") ||
			strings.Contains(s, "Permission to use, copy, modify, and distribute this software for any purpose"):
			add("ISC")
		case strings.Contains(s, "free and unencumbered software released into the public domain"):
			add("Unlicense")
		case strings.Contains(s, "SIL OPEN FONT LICENSE"):
			add("OFL-1.1")
		case strings.Contains(s, "This software is provided 'as-is'"):
			add("Zlib")
		case strings.Contains(s, "may you do good and not evil"):
			add("blessing (SQLite public domain)")
		}
	}
	if len(ids) == 0 {
		return "UNKNOWN"
	}
	return strings.Join(ids, " AND ")
}

// ---- rendering ---------------------------------------------------------------------------------------------------

func render(goComps, npmComps []*component) []byte {
	var b bytes.Buffer
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	w("# Third-party notices\n\n")
	w("<!-- Generated by scripts/release/notices (`make notices`). Do not edit by hand. -->\n\n")
	w("AstraTerm release binaries contain the third-party software listed below: the Go modules compiled into the\n")
	w("executable and the npm packages bundled into the embedded web UI. Each component is distributed under its own\n")
	w("license; the license texts shipped by each component are reproduced in [License texts](#license-texts).\n\n")

	all := append(slices.Clone(goComps), npmComps...)
	counts := map[string][2]int{}
	for _, c := range all {
		n := counts[c.License]
		if c.Kind == "go" {
			n[0]++
		} else {
			n[1]++
		}
		counts[c.License] = n
	}
	w("## Summary\n\n| License | Go modules | npm packages |\n|---|---:|---:|\n")
	for _, l := range sortedKeys(counts) {
		w("| %s | %d | %d |\n", l, counts[l][0], counts[l][1])
	}
	w("\n## Obligations beyond keeping the notices\n\n")
	var flagged []*component
	for _, c := range all {
		for _, id := range copyleft {
			if strings.Contains(c.License, id) {
				flagged = append(flagged, c)
				break
			}
		}
	}
	for _, c := range flagged {
		w("- **%s %s** (%s, %s)", c.Name, c.Version, c.License, kindLabel(c.Kind))
		switch {
		case strings.Contains(c.License, "MPL-2.0") && strings.Contains(c.License, " OR "):
			w(": used under the non-copyleft alternative of the dual license.\n")
		case strings.Contains(c.License, "MPL-2.0"):
			w(": file-level copyleft. It is distributed unmodified; its Source Code Form is available from the upstream\n"+
				"  project and the package registry (%s). Modified MPL files would have to be published under the MPL.\n", upstream(c))
		case strings.Contains(c.License, "OFL-1.1"):
			w(": font software; the license below must accompany the fonts, which may not be sold on their own.\n")
		default:
			w(": review the license terms below.\n")
		}
	}
	var unknown []string
	for _, c := range all {
		if c.License == "UNKNOWN" || len(c.Texts) == 0 {
			unknown = append(unknown, fmt.Sprintf("%s %s", c.Name, c.Version))
		}
	}
	if len(unknown) > 0 {
		w("- Components that ship **no license file** (license taken from their package metadata; notice to be\n  obtained from upstream): %s.\n", strings.Join(unknown, ", "))
	}
	w("\n%s\n", repoNotes)

	w("## Go modules\n\nCompiled into the `astraterm` executable (CGO disabled, statically linked).\n\n")
	w("| Module | Version | License | Platforms |\n|---|---|---|---|\n")
	for _, c := range goComps {
		plats := "all"
		if c.Platforms != nil {
			plats = strings.Join(c.Platforms, ", ")
		}
		w("| %s | %s | %s | %s |\n", c.Name, c.Version, c.License, plats)
	}
	w("\n## npm packages\n\nProduction dependencies from `web/package-lock.json`, bundled (minified) into the embedded web UI.\n\n")
	w("| Package | Version | License | Note |\n|---|---|---|---|\n")
	for _, c := range npmComps {
		w("| %s | %s | %s | %s |\n", c.Name, c.Version, c.License, c.Note)
	}

	// License texts, identical texts shown once.
	type group struct {
		file  string
		text  string
		users []string
	}
	var groups []*group
	index := map[string]*group{}
	for _, c := range all {
		for _, t := range c.Texts {
			g := index[t.Text]
			if g == nil {
				g = &group{file: t.File, text: t.Text}
				index[t.Text] = g
				groups = append(groups, g)
			}
			label := c.Name + " " + c.Version
			if !slices.Contains(g.users, label) {
				g.users = append(g.users, label)
			}
		}
	}
	w("\n## License texts\n")
	for _, g := range groups {
		w("\n### %s\n\n", strings.Join(g.users, ", "))
		fence := "```"
		for strings.Contains(g.text, fence) {
			fence += "`"
		}
		w("From `%s`:\n\n%stext\n%s\n%s\n", g.file, fence, g.text, fence)
	}
	return b.Bytes()
}

func upstream(c *component) string {
	if c.Kind == "npm" {
		return "https://www.npmjs.com/package/" + c.Name + "/v/" + c.Version
	}
	return "https://pkg.go.dev/" + c.Name + "@" + c.Version
}

func kindLabel(k string) string {
	if k == "go" {
		return "Go module"
	}
	return "npm package"
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
