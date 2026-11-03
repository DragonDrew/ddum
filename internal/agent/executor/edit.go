package executor

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// ReplaceYAMLScalar replaces the scalar that starts at line:column (1-based,
// as yaml.v3 reports) and currently reads oldValue with newValue, changing
// exactly that byte span so comments and formatting survive.
// A quoted scalar keeps its quotes.
func ReplaceYAMLScalar(data []byte, line, column int, oldValue, newValue string) ([]byte, error) {
	start, err := offsetOf(data, line, column)
	if err != nil {
		return nil, err
	}
	rest := data[start:]
	var end int
	switch {
	case len(rest) > 0 && (rest[0] == '"' || rest[0] == '\''):
		q := rest[0]
		i := 1
		for ; i < len(rest); i++ {
			if rest[i] == '\\' && q == '"' {
				i++
				continue
			}
			if rest[i] == q {
				if q == '\'' && i+1 < len(rest) && rest[i+1] == '\'' {
					i++ // '' escapes a quote in single-quoted scalars
					continue
				}
				break
			}
		}
		if i >= len(rest) {
			return nil, fmt.Errorf("unterminated quoted scalar at %d:%d", line, column)
		}
		inner := string(rest[1:i])
		if inner != oldValue || strings.ContainsAny(oldValue+newValue, `"'\`) {
			return nil, fmt.Errorf("scalar at %d:%d is %q, not the expected %q", line, column, inner, oldValue)
		}
		start++
		end = start + len(inner)
	default:
		if !bytes.HasPrefix(rest, []byte(oldValue)) {
			return nil, fmt.Errorf("scalar at %d:%d does not read %q", line, column, oldValue)
		}
		end = start + len(oldValue)
	}
	out := make([]byte, 0, len(data)-(end-start)+len(newValue))
	out = append(out, data[:start]...)
	out = append(out, newValue...)
	out = append(out, data[end:]...)
	return out, nil
}

// offsetOf converts a 1-based line and rune column to a byte offset.
func offsetOf(data []byte, line, column int) (int, error) {
	off := 0
	for l := 1; l < line; l++ {
		i := bytes.IndexByte(data[off:], '\n')
		if i < 0 {
			return 0, fmt.Errorf("line %d is past the end of the file", line)
		}
		off += i + 1
	}
	for c := 1; c < column; c++ {
		if off >= len(data) || data[off] == '\n' {
			return 0, fmt.Errorf("column %d is past the end of line %d", column, line)
		}
		_, size := utf8.DecodeRune(data[off:])
		off += size
	}
	return off, nil
}

// ReplaceEnvValue replaces the value on the last KEY= line, keeping its
// quoting, any "export " prefix, and every other line byte for byte.
func ReplaceEnvValue(data []byte, key, newValue string) ([]byte, error) {
	lines := bytes.SplitAfter(data, []byte("\n"))
	idx := -1
	for i, l := range lines {
		t := strings.TrimSpace(string(l))
		t = strings.TrimPrefix(t, "export ")
		k, _, ok := strings.Cut(t, "=")
		if ok && strings.TrimSpace(k) == key && !strings.HasPrefix(t, "#") {
			idx = i
		}
	}
	if idx < 0 {
		return nil, fmt.Errorf("%s is not defined in the environment file", key)
	}
	l := string(lines[idx])
	eq := strings.Index(l, "=")
	head, val := l[:eq+1], l[eq+1:]
	nl := ""
	switch {
	case strings.HasSuffix(val, "\r\n"):
		val, nl = val[:len(val)-2], "\r\n"
	case strings.HasSuffix(val, "\n"):
		val, nl = val[:len(val)-1], "\n"
	}
	lead := val[:len(val)-len(strings.TrimLeft(val, " \t"))]
	body := strings.TrimLeft(val, " \t")
	if strings.ContainsAny(newValue, "\"'\n#$ ") {
		return nil, fmt.Errorf("refusing to write an unusual tag value %q", newValue)
	}
	switch {
	case len(body) >= 2 && (body[0] == '"' || body[0] == '\''):
		q := body[0]
		if j := strings.IndexByte(body[1:], q); j >= 0 {
			body = string(q) + newValue + string(q) + body[j+2:]
			break
		}
		fallthrough
	default:
		// Keep a trailing " # comment".
		comment := ""
		if j := strings.Index(body, " #"); j >= 0 {
			comment = body[j:]
		}
		body = newValue + comment
	}
	lines[idx] = []byte(head + lead + body + nl)
	return bytes.Join(lines, nil), nil
}

// applyEdit changes one tag in place.
func applyEdit(e edit) error {
	if e.variable != "" {
		return editFile(e.file, func(b []byte) ([]byte, error) { return ReplaceEnvValue(b, e.variable, e.newValue) })
	}
	return editFile(e.file, func(b []byte) ([]byte, error) {
		return ReplaceYAMLScalar(b, e.line, e.column, e.oldValue, e.newValue)
	})
}

// editFile applies edit to a file and writes it back like replaceFile.
func editFile(path string, edit func([]byte) ([]byte, error)) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, err := edit(data)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return replaceFile(path, out)
}

// replaceFile atomically replaces a file the owner keeps. The agent runs as
// root, so a plain rename would leave a compose file or .env owned by root,
// which its owner may then not even be able to read, and would turn a symlink
// into a detached copy. So it writes through symlinks and keeps the permissions
// and the owner.
func replaceFile(path string, data []byte) error {
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	st, err := os.Stat(target)
	if err != nil {
		return err
	}
	return writeAtomic(target, data, st.Mode().Perm(), func(tmp *os.File) { copyOwner(tmp, st) })
}
