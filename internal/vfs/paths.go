package vfs

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxPathLen bounds API paths.
const maxPathLen = 4096

// partSuffix is appended to upload targets while data is being received (atomic rename on completion).
const partSuffix = ".nexterm-part"

// cleanPath validates and normalizes an API path: it must be valid UTF-8 without NUL bytes; "~" and "~/x" are relative
// to home, other relative paths are resolved against home; the result is absolute and clean.
func cleanPath(p, home string) (string, error) {
	if len(p) > maxPathLen {
		return "", fmt.Errorf("path is too long")
	}
	if strings.IndexByte(p, 0) >= 0 || !utf8.ValidString(p) {
		return "", fmt.Errorf("invalid path")
	}
	p = strings.TrimSpace(p)
	switch {
	case p == "":
		p = home
	case p == "~":
		p = home
	case strings.HasPrefix(p, "~/"):
		p = path.Join(home, p[2:])
	case !strings.HasPrefix(p, "/"):
		p = path.Join(home, p)
	}
	if p == "" {
		p = "/"
	}
	return path.Clean("/" + strings.TrimPrefix(p, "/")), nil
}

// parentDir returns the parent of a clean absolute path ("/" for "/").
func parentDir(p string) string {
	if p == "/" || p == "" {
		return "/"
	}
	return path.Dir(p)
}

// baseName returns the last element of a clean absolute path ("/" for "/").
func baseName(p string) string {
	if p == "/" || p == "" {
		return "/"
	}
	return path.Base(p)
}

// joinPath joins a clean absolute dir and a name.
func joinPath(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}

// validName reports whether name is a single path element usable for a new file.
func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00") && utf8.ValidString(name)
}

// isWithin reports whether p equals dir or lies below it.
func isWithin(p, dir string) bool {
	if dir == "/" {
		return true
	}
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// relTo returns p relative to dir (p must be within dir); "" for dir itself.
func relTo(p, dir string) string {
	if p == dir {
		return ""
	}
	if dir == "/" {
		return strings.TrimPrefix(p, "/")
	}
	return strings.TrimPrefix(p, dir+"/")
}

// ---- chmod modes --------------------------------------------------------------------------------------------------

// modeSpec is a parsed chmod mode: an absolute octal mode or a list of symbolic clauses.
type modeSpec struct {
	abs     bool
	perm    uint32
	clauses []modeClause
	text    string // validated chmod(1) mode text (for chmod -R over exec)
}

// chmodArg renders the spec for chmod(1).
func (m modeSpec) chmodArg() string {
	if m.abs {
		return fmt.Sprintf("%04o", m.perm)
	}
	return m.text
}

type modeClause struct {
	who uint32 // mask of affected bits (u: 0o4700, g: 0o2070, o: 0o1007, a: all); 0 = "a" honoring umask semantics
	ops []modeOp
}

type modeOp struct {
	op    byte   // '+', '-', '='
	bits  uint32 // rwx/st bits expressed for all classes (masked by who)
	xcond bool   // X: execute only for directories or files already executable by someone
	copy  byte   // 'u', 'g', 'o' when copying permissions from a class
}

// parseMode parses an octal ("755", "0755", "4755") or symbolic ("u+x,go-w", "a=rX") chmod mode.
func parseMode(s string) (modeSpec, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return modeSpec{}, fmt.Errorf("empty mode")
	}
	if s[0] >= '0' && s[0] <= '7' {
		v, err := strconv.ParseUint(s, 8, 32)
		if err != nil || v > 0o7777 {
			return modeSpec{}, fmt.Errorf("invalid octal mode %q", s)
		}
		return modeSpec{abs: true, perm: uint32(v)}, nil
	}
	var spec modeSpec
	var text []string
	for _, part := range strings.Split(s, ",") {
		if part == "" {
			return modeSpec{}, fmt.Errorf("invalid mode %q", s)
		}
		var cl modeClause
		i := 0
		for ; i < len(part); i++ {
			switch part[i] {
			case 'u':
				cl.who |= 0o4700
			case 'g':
				cl.who |= 0o2070
			case 'o':
				cl.who |= 0o1007
			case 'a':
				cl.who |= 0o7777
			default:
				goto ops
			}
		}
	ops:
		if cl.who == 0 {
			// No class given: like "a" (the umask is not applied, matching apply). Spelled out for chmod(1), which
			// would otherwise honor the umask — and take a leading "-w" for an option.
			cl.who = 0o7777
			part, i = "a"+part, i+1
		}
		text = append(text, part)
		if i >= len(part) {
			return modeSpec{}, fmt.Errorf("invalid mode %q", s)
		}
		for i < len(part) {
			c := part[i]
			if c != '+' && c != '-' && c != '=' {
				return modeSpec{}, fmt.Errorf("invalid mode %q", s)
			}
			op := modeOp{op: c}
			i++
			for ; i < len(part); i++ {
				switch part[i] {
				case 'r':
					op.bits |= 0o444
				case 'w':
					op.bits |= 0o222
				case 'x':
					op.bits |= 0o111
				case 'X':
					op.xcond = true
				case 's':
					op.bits |= 0o6000
				case 't':
					op.bits |= 0o1000
				case 'u', 'g', 'o':
					if op.copy != 0 || op.bits != 0 || op.xcond {
						return modeSpec{}, fmt.Errorf("invalid mode %q", s)
					}
					op.copy = part[i]
				default:
					goto next
				}
			}
		next:
			cl.ops = append(cl.ops, op)
		}
		spec.clauses = append(spec.clauses, cl)
	}
	spec.text = strings.Join(text, ",")
	return spec, nil
}

// apply computes the new permission bits for a file with current st_mode cur.
func (m modeSpec) apply(cur uint32, isDir bool) uint32 {
	if m.abs {
		return m.perm
	}
	perm := cur & 0o7777
	for _, cl := range m.clauses {
		for _, op := range cl.ops {
			bits := op.bits
			if op.xcond && (isDir || perm&0o111 != 0) {
				bits |= 0o111
			}
			if op.copy != 0 {
				var src uint32
				switch op.copy {
				case 'u':
					src = (perm >> 6) & 7
				case 'g':
					src = (perm >> 3) & 7
				case 'o':
					src = perm & 7
				}
				bits = src<<6 | src<<3 | src
			}
			bits &= cl.who
			switch op.op {
			case '+':
				perm |= bits
			case '-':
				perm &^= bits
			case '=':
				// "=" clears the rwx bits of the affected classes (and their special bit) before setting.
				clear := cl.who & 0o777
				if cl.who&0o700 != 0 {
					clear |= 0o4000
				}
				if cl.who&0o070 != 0 {
					clear |= 0o2000
				}
				if cl.who&0o007 != 0 {
					clear |= 0o1000
				}
				perm = perm&^clear | bits
			}
		}
	}
	return perm
}
