// Package uci 解析 OpenWrt 的 UCI 配置文件。
//
// 只做解析，不做写入：smsrelay 的配置由 LuCI（rpcd/uci）负责写，
// 守护进程只读。这样配置的"真相"只有一份，也不会出现两个进程同时改配置。
//
// 语法范围（覆盖 UCI 实际会产出的全部形式）：
//
//	config <type> [<name>]
//	    option <key> <value>
//	    list   <key> <value>
//
// 值可以带单引号、双引号或不带引号；单引号内支持 \' 转义，双引号内支持 \" 与 \\。
// 注释必须出现在行首（前面可以有空白），不支持行尾注释。
package uci

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// Section 是配置里的一个段。
// Name 对匿名段为空；Options 与 Lists 按出现顺序保存全部键值。
type Section struct {
	Type    string
	Name    string
	Options map[string]string
	Lists   map[string][]string
}

// Get 取一个 option 的值，不存在时返回空串。
func (s *Section) Get(key string) string {
	if s == nil {
		return ""
	}
	return s.Options[key]
}

// List 取一个 list 的全部值。
func (s *Section) List(key string) []string {
	if s == nil {
		return nil
	}
	return s.Lists[key]
}

// File 是一份解析后的 UCI 配置。
type File struct {
	Sections []*Section
}

// Load 从文件读取并解析。文件不存在不算错误，返回空配置。
func Load(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &File{}, nil
		}
		return nil, err
	}
	defer f.Close()
	file, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return file, nil
}

// Parse 解析 UCI 文本。
func Parse(r io.Reader) (*File, error) {
	out := &File{}
	var cur *Section

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields, err := splitFields(line)
		if err != nil {
			return nil, fmt.Errorf("第 %d 行: %w", lineNo, err)
		}
		if len(fields) == 0 {
			continue
		}

		switch fields[0] {
		case "config":
			if len(fields) < 2 {
				return nil, fmt.Errorf("第 %d 行: config 缺少段类型", lineNo)
			}
			cur = &Section{
				Type:    fields[1],
				Options: make(map[string]string),
				Lists:   make(map[string][]string),
			}
			if len(fields) >= 3 {
				cur.Name = fields[2]
			}
			out.Sections = append(out.Sections, cur)

		case "option", "list":
			if cur == nil {
				return nil, fmt.Errorf("第 %d 行: %s 出现在任何 config 之前", lineNo, fields[0])
			}
			if len(fields) < 2 {
				return nil, fmt.Errorf("第 %d 行: %s 缺少键名", lineNo, fields[0])
			}
			key := fields[1]
			val := ""
			if len(fields) >= 3 {
				val = fields[2]
			}
			if fields[0] == "option" {
				cur.Options[key] = val
			} else {
				cur.Lists[key] = append(cur.Lists[key], val)
			}

		default:
			return nil, fmt.Errorf("第 %d 行: 无法识别 %q", lineNo, fields[0])
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// SectionsOf 返回指定类型的全部段（保持出现顺序）。
func (f *File) SectionsOf(typ string) []*Section {
	if f == nil {
		return nil
	}
	var out []*Section
	for _, s := range f.Sections {
		if s.Type == typ {
			out = append(out, s)
		}
	}
	return out
}

// First 返回指定类型的第一个段，没有则 nil。
func (f *File) First(typ string) *Section {
	if f == nil {
		return nil
	}
	for _, s := range f.Sections {
		if s.Type == typ {
			return s
		}
	}
	return nil
}

// splitFields 按空白切分一行，并处理引号与转义。
//
// UCI 的引号规则比 shell 简单得多：引号只包裹整段值，不做变量展开，
// 也不会把相邻的引号拼接起来。这里按同样的语义实现。
func splitFields(line string) ([]string, error) {
	var (
		fields []string
		buf    strings.Builder
		inWord bool
		quote  byte
	)
	flush := func() {
		if inWord {
			fields = append(fields, buf.String())
			buf.Reset()
			inWord = false
		}
	}

	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0:
			if c == '\\' && i+1 < len(line) {
				n := line[i+1]
				// 只对引号本身和反斜杠做转义，其余原样保留（UCI 不做 \n 展开）
				if n == quote || n == '\\' {
					buf.WriteByte(n)
					i++
					continue
				}
				buf.WriteByte(c)
				continue
			}
			if c == quote {
				quote = 0
				continue
			}
			buf.WriteByte(c)

		case c == '\'' || c == '"':
			quote = c
			inWord = true

		case c == ' ' || c == '\t':
			flush()

		default:
			buf.WriteByte(c)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("引号未闭合")
	}
	flush()
	return fields, nil
}
