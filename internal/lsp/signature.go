package lsp

import "strings"

type signatureParameter struct {
	label      string
	name       string
	positional bool
	keyword    bool
	variadic   bool
	keywords   bool
}

type parsedSignature struct {
	parameters []signatureParameter
}

func parseSignature(signature string) (parsedSignature, bool) {
	signature = strings.TrimSpace(signature)
	if signature == "" || signature[0] != '(' {
		return parsedSignature{}, false
	}
	closing, ok := matchingParenthesis(signature)
	if !ok {
		return parsedSignature{}, false
	}
	if trailing := strings.TrimSpace(signature[closing+1:]); trailing != "" && !strings.HasPrefix(trailing, "->") {
		return parsedSignature{}, false
	}
	parts, ok := splitSignatureParameters(signature[1:closing])
	if !ok {
		return parsedSignature{}, false
	}
	parameters := make([]signatureParameter, 0, len(parts))
	keywordOnly := false
	for _, raw := range parts {
		label := strings.TrimSpace(raw)
		if label == "" {
			continue
		}
		switch label {
		case "/":
			for index := range parameters {
				parameters[index].keyword = false
			}
			continue
		case "*":
			keywordOnly = true
			continue
		}
		parameter := signatureParameter{label: label, positional: !keywordOnly, keyword: true}
		nameText := label
		if strings.HasPrefix(nameText, "**") {
			parameter.positional = false
			parameter.keywords = true
			keywordOnly = true
			nameText = strings.TrimPrefix(nameText, "**")
		} else if strings.HasPrefix(nameText, "*") {
			parameter.positional = true
			parameter.keyword = false
			parameter.variadic = true
			keywordOnly = true
			nameText = strings.TrimPrefix(nameText, "*")
		}
		parameter.name = signatureParameterName(nameText)
		if parameter.name == "" {
			return parsedSignature{}, false
		}
		parameters = append(parameters, parameter)
	}
	return parsedSignature{parameters: parameters}, true
}

func matchingParenthesis(signature string) (int, bool) {
	stack := make([]byte, 0, 8)
	var quote byte
	escaped := false
	for index := 0; index < len(signature); index++ {
		value := signature[index]
		if quote != 0 {
			if escaped {
				escaped = false
			} else if value == '\\' {
				escaped = true
			} else if value == quote {
				quote = 0
			}
			continue
		}
		if value == '\'' || value == '"' {
			quote = value
			continue
		}
		switch value {
		case '(', '[', '{':
			stack = append(stack, value)
		case ')', ']', '}':
			if len(stack) == 0 || !matchingDelimiter(stack[len(stack)-1], value) {
				return 0, false
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return index, value == ')'
			}
		}
	}
	return 0, false
}

func splitSignatureParameters(content string) ([]string, bool) {
	if strings.TrimSpace(content) == "" {
		return nil, true
	}
	parts := make([]string, 0, 8)
	start := 0
	stack := make([]byte, 0, 8)
	var quote byte
	escaped := false
	for index := 0; index < len(content); index++ {
		value := content[index]
		if quote != 0 {
			if escaped {
				escaped = false
			} else if value == '\\' {
				escaped = true
			} else if value == quote {
				quote = 0
			}
			continue
		}
		if value == '\'' || value == '"' {
			quote = value
			continue
		}
		switch value {
		case '(', '[', '{':
			stack = append(stack, value)
		case ')', ']', '}':
			if len(stack) == 0 || !matchingDelimiter(stack[len(stack)-1], value) {
				return nil, false
			}
			stack = stack[:len(stack)-1]
		case ',':
			if len(stack) == 0 {
				parts = append(parts, content[start:index])
				start = index + 1
			}
		}
	}
	if quote != 0 || len(stack) != 0 {
		return nil, false
	}
	parts = append(parts, content[start:])
	return parts, true
}

func matchingDelimiter(opening, closing byte) bool {
	return opening == '(' && closing == ')' || opening == '[' && closing == ']' || opening == '{' && closing == '}'
}

func signatureParameterName(label string) string {
	end := len(label)
	for index, value := range label {
		if value == ':' || value == '=' || value == ' ' || value == '\t' {
			end = index
			break
		}
	}
	return strings.TrimSpace(label[:end])
}

func (signature parsedSignature) activeParameter(positional int, keyword string) (int, bool) {
	if keyword != "" {
		fallback := -1
		for index, parameter := range signature.parameters {
			if parameter.keyword && parameter.name == keyword {
				return index, true
			}
			if parameter.keywords {
				fallback = index
			}
		}
		return fallback, fallback >= 0
	}
	position := 0
	for index, parameter := range signature.parameters {
		if !parameter.positional {
			continue
		}
		if parameter.variadic || position == positional {
			return index, true
		}
		position++
	}
	return 0, false
}
