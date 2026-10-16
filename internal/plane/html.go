package plane

import (
	"html"
	"regexp"
	"strings"
)

// blockTags gain a newline before and after their content. Mirrors the block
// set of the retired plane-intake.py HTML parser.
var blockTags = map[string]bool{
	"p": true, "div": true, "br": true, "li": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"pre": true, "tr": true, "blockquote": true,
}

var (
	bulletNewlines = regexp.MustCompile(`(?m)^(\s*- )\s*\n+\s*`)
	trailingSpaces = regexp.MustCompile(`[ \t]+\n`)
	excessNewlines = regexp.MustCompile(`\n{3,}`)
)

// HTMLToText renders Plane's description HTML to plain text: headings become
// "# ", list items "- ", inline code gains backticks, and checkboxes become
// "[x]"/"[ ]". Whitespace is normalised like the legacy Python parser.
func HTMLToText(input string) string {
	if input == "" {
		return ""
	}
	var out strings.Builder
	for index := 0; index < len(input); {
		if input[index] != '<' {
			next := strings.IndexByte(input[index:], '<')
			if next < 0 {
				out.WriteString(html.UnescapeString(input[index:]))
				break
			}
			out.WriteString(html.UnescapeString(input[index : index+next]))
			index += next
			continue
		}
		end := strings.IndexByte(input[index:], '>')
		if end < 0 {
			// Unterminated tag: keep the remainder as text.
			out.WriteString(html.UnescapeString(input[index:]))
			break
		}
		raw := strings.TrimSpace(input[index+1 : index+end])
		index += end + 1
		writeTag(&out, raw)
	}
	text := out.String()
	text = bulletNewlines.ReplaceAllString(text, "$1")
	text = trailingSpaces.ReplaceAllString(text, "\n")
	text = excessNewlines.ReplaceAllString(text, "\n\n")
	return strings.TrimSpace(text)
}

// writeTag emits the text a start or end tag contributes.
func writeTag(out *strings.Builder, raw string) {
	if raw == "" || strings.HasPrefix(raw, "!") {
		return
	}
	if strings.HasPrefix(raw, "/") {
		tag := tagName(raw[1:])
		if tag == "code" {
			out.WriteString("`")
		}
		if blockTags[tag] {
			out.WriteString("\n")
		}
		return
	}
	// A start tag. A self-closing slash is irrelevant for the tags we track.
	name := tagName(raw)
	if blockTags[name] {
		out.WriteString("\n")
	}
	switch name {
	case "li":
		out.WriteString("- ")
	case "h1", "h2", "h3":
		out.WriteString(strings.Repeat("#", int(name[1]-'0')) + " ")
	case "code":
		out.WriteString("`")
	case "input":
		if inputType(raw) == "checkbox" {
			if hasAttribute(raw, "checked") {
				out.WriteString("[x] ")
			} else {
				out.WriteString("[ ] ")
			}
		}
	}
}

// tagName extracts the leading tag name from the inside of a tag.
func tagName(raw string) string {
	raw = strings.TrimSpace(raw)
	end := 0
	for end < len(raw) {
		character := raw[end]
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' {
			end++
			continue
		}
		break
	}
	return strings.ToLower(raw[:end])
}

// attributeValue returns the value of an attribute in a tag body.
func attributeValue(raw, attribute string) (string, bool) {
	lower := strings.ToLower(raw)
	index := 0
	for {
		found := strings.Index(lower[index:], attribute)
		if found < 0 {
			return "", false
		}
		found += index
		before := byte(' ')
		if found > 0 {
			before = lower[found-1]
		}
		after := byte(' ')
		if found+len(attribute) < len(lower) {
			after = lower[found+len(attribute)]
		}
		if isSpace(before) && (isSpace(after) || after == '=') {
			rest := strings.TrimSpace(raw[found+len(attribute):])
			if rest == "" || rest[0] != '=' {
				return "", true
			}
			rest = strings.TrimSpace(rest[1:])
			if rest == "" {
				return "", true
			}
			if rest[0] == '"' || rest[0] == '\'' {
				quote := rest[0]
				if close := strings.IndexByte(rest[1:], quote); close >= 0 {
					return rest[1 : 1+close], true
				}
				return rest[1:], true
			}
			end := 0
			for end < len(rest) && !isSpace(rest[end]) && rest[end] != '>' {
				end++
			}
			return rest[:end], true
		}
		index = found + len(attribute)
	}
}

func inputType(raw string) string {
	value, _ := attributeValue(raw, "type")
	return strings.ToLower(value)
}

func hasAttribute(raw, attribute string) bool {
	_, ok := attributeValue(raw, attribute)
	return ok
}

func isSpace(character byte) bool {
	return character == ' ' || character == '\t' || character == '\n' || character == '\r'
}
