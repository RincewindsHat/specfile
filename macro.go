package specfile

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	dirutils "github.com/marguerite/util/dir"
	"github.com/marguerite/util/slice"
)

var (
	macroDirs   = []string{"/usr/lib/rpm/macros.d", "/etc/rpm"}
	macroFiles  = []string{"/usr/lib/rpm/macros", "/usr/lib/rpm/suse/macros"}
	buildConfig = "/usr/lib/build/configs/default.conf"
)

func getFunctionName(str string) string {
	tmp := make([]byte, 0, len(str))
	var i int
	for _, b := range []byte(str) {
		if b == '(' {
			break
		}
		tmp[i] = b
		i++
	}
	return string(tmp)
}

// Macros rpm macros
type Macros []Macro

// Find find a specific macro through macros
func (macros Macros) Find(m Macro) int {
	for i, v := range macros {
		if v.Condition == m.Condition {
			if (v.Type == "variable" && v.Name == m.Name) || (v.Type == "function" && getFunctionName(v.Name) == m.Name) {
				return i
			}
		}
	}
	return -1
}

// Concat concat two macro slice
func (macros *Macros) Concat(macros1 Macros) {
	for _, v := range macros1 {
		if i := macros.Find(v); i >= 0 {
			(*macros)[i].Update(v.Value)
		} else {
			*macros = append(*macros, v)
		}
	}
}

// Macro represent a rpm macro
type Macro struct {
	Indicator string // %global or %define
	Type      string // function or variable
	item
}

// Parse actually parse the macro
func (m *Macro) Parse(str string) error {
	var indicator, name string
	var tmp []byte

	bytes := []byte(str)

	for i := 0; i < len(bytes); i++ {
		if i == 0 && bytes[i] != '%' {
			return fmt.Errorf("not a macro")
		}
		if bytes[i] == '\\' {
			name = string(tmp)
			break
		}
		if bytes[i] == ' ' || bytes[i] == '\t' {
			if string(tmp) == "%global" || string(tmp) == "%define" {
				indicator = string(tmp)
				tmp = []byte{}
				continue
			} else {
				name = string(tmp)
				break
			}
		}
		tmp = append(tmp, bytes[i])
	}

	m.Indicator = indicator
	m.Name = name
	if strings.Contains(name, "(") {
		m.Type = "function"
	} else {
		m.Type = "variable"
	}
	tmp1 := name
	if len(indicator) > 0 {
		tmp1 = indicator + " " + tmp1
	}
	str = strings.Replace(str, tmp1, "", 1)
	str = strings.TrimLeft(str, "\\")
	m.Value = strings.TrimSpace(str)
	m.Name = strings.Replace(m.Name, "%", "", 1)
	return nil
}

// Update update macro definition
func (m *Macro) Update(val string) {
	m.Value = val
}

// initSystemMacros load system defined rpm macros
func initSystemMacros() Macros {
	var macros Macros
	var files []string
	for _, v := range macroDirs {
		dirs, err1 := dirutils.Glob(filepath.Join(v, "macros.*"))
		if err1 != nil {
			fmt.Printf("can not find macros in %s\n", v)
			continue
		}
		slice.Concat(&files, dirs)
	}
	slice.Concat(&macroFiles, files)
	slice.Concat(&macroFiles, buildConfig)

	for _, v := range macroFiles {
		f, err1 := os.Open(v)
		if err1 != nil {
			fmt.Printf("can not open %s\n", v)
			continue
		}

		var tmp Macros
		if strings.HasSuffix(v, ".conf") {
			tmp, err1 = parseBuildConfig(f)
		} else {
			tmp, err1 = parseMacroFile(f)
		}

		if err1 != nil {
			fmt.Printf("failed to parse %s, %v\n", v, err1)
			break
		}
		macros.Concat(tmp)
		f.Close()
	}

	return macros
}

// parseMacroFile parse .macros file
func parseMacroFile(f io.ReaderAt) (Macros, error) {
	var macros Macros
	err := walkFile(f, false, func(rd io.ReaderAt, line *Line) (error, int64) {
		// skip comment and empty line
		if !strings.HasPrefix(line.Last, "#") && line.Lines[0] != "\n" && len(line.Lines[0]) != 0 {
			var m Macro
			err1 := (&m).Parse(strings.Join(line.Lines, ""))
			// not a macro
			if err1 != nil {
				return err1, line.Offset
			}
			macros = append(macros, m)
		}
		return nil, line.Offset
	}, "Parentheses")

	return macros, err
}

// parseBuildConfig parse /usr/lib/build/configs/default.conf openSUSE specific place for some macros
func parseBuildConfig(f io.ReaderAt) (Macros, error) {
	var macros Macros
	isMacro := false
	err := walkFile(f, false, func(rd io.ReaderAt, line *Line) (error, int64) {
		var macro Macro
		str := strings.Join(line.Lines, "")
		if strings.HasPrefix(str, "%define") {
			err1 := (&macro).Parse(str)
			if err1 != nil {
				return err1, line.Offset
			}
			macros = append(macros, macro)
		}
		if strings.HasPrefix(line.Last, "Macros:") {
			isMacro = true
			return nil, line.Offset
		}
		if strings.HasPrefix(line.Last, ":Macros") {
			isMacro = false
		}

		if isMacro {
			// skip comment and empty line
			if !strings.HasPrefix(line.Last, "#") && line.Lines[0] != "\n" && len(line.Lines[0]) != 0 {
				err1 := (&macro).Parse(str)
				if err1 != nil {
					return err1, line.Offset
				}
				macros = append(macros, macro)
			}
		}

		return nil, line.Offset
	}, "Parentheses")
	return macros, err
}

func expandMacro(macro Macro, system, local Macros, tags []Tag) string {
	// no macro at all
	str := macro.Value
	if !strings.Contains(str, "%") || macro.Type == "function" {
		return str
	}
	if strings.Contains(str, "expand:") {
		str = expand(str)
	}

	var start, useCounter bool
	var idx, j, n int
	var c Counter

	tmp := make([]byte, 0, len(str))
	// usuall you will not see a macro body containing more than 30 other macros
	records := make([]string, 0, 30)

	for i, v := range []byte(str) {
		if v == '%' {
			start = true
			idx = i
		}
		if start {
			// don't allow nested macro, find the most inner macro first
			if v == '%' {
				tmp[0] = '%'
				j++
				idx = i
				useCounter = false
				continue
			}
			tmp[j] = v
			j++
			// the next is '{' or '(', we should find the corresponding '}' or ')' to close
			if i == idx+1 && (v == '{' || v == '(') {
				useCounter = true
			}
			// eg '%ix86 x86_64 %arm' stop at whitespace or end of str
			r, _ := utf8.DecodeRune([]byte{v})

			if !useCounter && (unicode.IsSpace(r) || i == len(str)-1) {
				// the space was appended to tmp
				records[n] = string(tmp)
				n++
				tmp = make([]byte, 0, len(str)-1-i)
				start = false
			}
			if useCounter {
				c.Count(tmp)
				if c.Valid() {
					records[n] = string(tmp)
					n++
					tmp = make([]byte, 0, len(str)-1-i)
					useCounter = false
					start = false
				}
				c.Reset()
			}
		}
	}

	for _, v := range records {
		str = strings.Replace(str, v, fillupMacroWithValue(v, system, local, tags), 1)
	}

	// the outer
	if strings.Contains(trim(str), "%") {
		newMacro := macro
		newMacro.Value = str
		newMacro.Type = "variable"
		str = expandMacro(newMacro, system, local, tags)
	}

	if len(str) > 1 {
		// shell commands
		if str[1] == '(' {
			str = callShell(trim(str))
		}
		// macro function
		if str[1] == '{' {
			str = execMacroFunction(str, system, local)
			newMacro := macro
			newMacro.Value = str
			newMacro.Type = "variable"
			str = expandMacro(newMacro, system, local, tags)
		}
	}
	return str
}

func execMacroFunction(s string, system, local Macros) string {
	str := trim(s)
	arr := strings.Split(str, " ")

	if arr[0] == str {
		// not a macro function
		return s
	}

	name := arr[0]
	num := len(arr) - 1
	if i := local.Find(Macro{"", "", item{name, "", "", "", nil}}); i >= 0 {
		val := local[i].Value
		for j := 1; j <= num; j++ {
			if strings.Contains(val, "%{"+strconv.Itoa(j)+"}") {
				val = strings.Replace(val, "%{"+strconv.Itoa(j)+"}", arr[j], -1)
			}
		}
		return val
	}
	return ""
}

func callShell(str string) string {
	out, err := exec.Command("/bin/sh", "-c", str).Output()
	if err != nil {
		panic(err)
	}
	return strings.TrimSpace(string(out))
}

// newExpandReplacer build a new strings.Replacer
func newExpandReplacer(percent bool) *strings.Replacer {
	arr := []string{"expand:", "", "%%", ""}
	if percent {
		arr[0] = "%expand:"
	}
	return strings.NewReplacer(arr...)
}

// expand implementation of rpm %{expand: }
func expand(str string) string {
	idx := strings.LastIndex(str, "expand:")

	if idx < 0 {
		return str
	}

	// without {, there must be only one expand
	// usually str[idx-1] is '{'
	if str[idx-1] == '%' {
		return newExpandReplacer(true).Replace(str)
	}

	var c Counter

	arr := make([]byte, 2, len(str))
	arr[0] = '%'
	arr[1] = '{'
	j := 2

	for i := idx; i < len(str); i++ {
		arr[j] = str[i]
		c.Count(arr)
		if c.Valid() {
			break
		}
		c.Reset()
                j++
	}

	s := string(arr)
	tmp := trim(newExpandReplacer(false).Replace(s))
	str = strings.Replace(str, s, tmp, 1)

	if strings.Contains(str, "expand:") {
		str = expand(str)
	}

	return str
}

// trim trim the surrounding "%{}"
func trim(str string) string {
	str = strings.TrimLeftFunc(str, func(r rune) bool {
		return r == '%' || r == '{' || r == '('
	})
	return strings.TrimRightFunc(str, func(r rune) bool {
		return r == '}' || r == ')'
	})
}

// splitConditionalMacro split conditional macro like "%{!?version:5}" or "%{?version}"
// to the macro "version", default value "5", and a status symbol `stat` (> 0 means ?, < 0 means !?, = 0 means no such prefix)
func splitConditionalMacro(str string) (string, string, int) {
	str = trim(str)
	stat := 0

	var defaultValue string

	// do the ?! and ? judge
	if strings.HasPrefix(str, "!?") {
		stat = -1
		str = strings.TrimPrefix(str, "!?")
	}
	if strings.HasPrefix(str, "?") {
		stat = 1
		str = strings.TrimPrefix(str, "?")
	}

	if strings.Contains(str, ":") {
		arr := strings.Split(str, ":")
		if arr[0] != str {
			str = arr[0]
			defaultValue = arr[1]
		}
	}

	return str, defaultValue, stat
}

func fillupMacroWithValue(str string, system, local Macros, tags []Tag) string {
	str, defaultValue, stat := splitConditionalMacro(str)

	if i := local.Find(Macro{"", "", item{str, "", "", "", nil}}); i >= 0 {
		if stat < 0 {
			return ""
		}
		return local[i].Value
	}
	if i := system.Find(Macro{"", "", item{str, "", "", "", nil}}); i >= 0 {
		if stat < 0 {
			return ""
		}
		return system[i].Value
	}
	// things like %{name} or %name
	for _, t := range tags {
		if str == strings.ToLower(t.Name) {
			if stat < 0 {
				return ""
			}
			return t.Value
		}
	}
	if stat < 0 {
		return defaultValue
	}
	return ""
}
