package python

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	maxWorkerEnvironmentFileSize  = 256 * 1024
	maxWorkerEnvironmentCount     = 256
	maxWorkerEnvironmentKeySize   = 256
	maxWorkerEnvironmentValueSize = 16 * 1024
	maxWorkerEnvironmentUnits     = 30_000
)

type workerEnvironmentEntry struct {
	name  string
	value string
}

var reservedWorkerEnvironmentNames = map[string]struct{}{
	"pythondontwritebytecode": {},
	"pythonunbuffered":        {},
	"pythonutf8":              {},
	"tmpdir":                  {},
	"tmp":                     {},
	"temp":                    {},
	"systemroot":              {},
}

func loadWorkerEnvironment(config Config, logger Logger) ([]workerEnvironmentEntry, error) {
	values := make(map[string]workerEnvironmentEntry)
	if config.EnvironmentFile != "" {
		parsed, err := readWorkerEnvironmentFile(config.EnvironmentFile, logger)
		if err != nil {
			return nil, err
		}
		for _, entry := range parsed {
			values[normalizeEnvironmentKey(entry.name)] = entry
		}
	}

	overrides := make([]string, 0, len(config.Environment))
	for name := range config.Environment {
		overrides = append(overrides, name)
	}
	sort.Slice(overrides, func(left, right int) bool {
		leftKey := normalizeEnvironmentKey(overrides[left])
		rightKey := normalizeEnvironmentKey(overrides[right])
		if leftKey == rightKey {
			return overrides[left] < overrides[right]
		}
		return leftKey < rightKey
	})
	seenOverrides := make(map[string]string, len(overrides))
	for _, name := range overrides {
		if err := validateWorkerEnvironmentName(name); err != nil {
			return nil, fmt.Errorf("worker environment override %q: %w", name, err)
		}
		normalized := normalizeEnvironmentKey(name)
		if previous, exists := seenOverrides[normalized]; exists {
			return nil, fmt.Errorf("worker environment overrides %q and %q refer to the same variable", previous, name)
		}
		seenOverrides[normalized] = name
		value := config.Environment[name]
		if value == nil {
			delete(values, normalized)
			continue
		}
		if err := validateWorkerEnvironmentValue(*value); err != nil {
			return nil, fmt.Errorf("worker environment override %q: %w", name, err)
		}
		values[normalized] = workerEnvironmentEntry{name: name, value: *value}
	}

	entries := make([]workerEnvironmentEntry, 0, len(values))
	for _, entry := range values {
		entries = append(entries, entry)
	}
	sortWorkerEnvironment(entries)
	if err := validateWorkerEnvironment(entries); err != nil {
		return nil, err
	}
	if logger != nil {
		names := make([]string, len(entries))
		for index, entry := range entries {
			names[index] = entry.name
		}
		logger.Infof("worker environment source=%q keys=%s count=%d inherited=PATH", config.EnvironmentFile, strings.Join(names, ","), len(entries))
	}
	return entries, nil
}

func readWorkerEnvironmentFile(path string, logger Logger) ([]workerEnvironmentEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open worker environment file %q: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect worker environment file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("worker environment file %q is not a regular file", path)
	}
	if info.Size() > maxWorkerEnvironmentFileSize {
		return nil, fmt.Errorf("worker environment file %q exceeds %d bytes", path, maxWorkerEnvironmentFileSize)
	}
	if workerEnvironmentFileIsBroadlyReadable(info.Mode()) && logger != nil {
		logger.Warningf("worker environment file %q is group- or world-readable", path)
	}
	payload, err := io.ReadAll(io.LimitReader(file, maxWorkerEnvironmentFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read worker environment file %q: %w", path, err)
	}
	if len(payload) > maxWorkerEnvironmentFileSize {
		return nil, fmt.Errorf("worker environment file %q exceeds %d bytes", path, maxWorkerEnvironmentFileSize)
	}
	return parseWorkerEnvironment(path, payload)
}

func parseWorkerEnvironment(path string, payload []byte) ([]workerEnvironmentEntry, error) {
	if !utf8.Valid(payload) {
		return nil, fmt.Errorf("worker environment file %q contains invalid UTF-8", path)
	}
	if bytes.IndexByte(payload, 0) >= 0 {
		return nil, fmt.Errorf("worker environment file %q contains NUL", path)
	}
	if bytes.HasPrefix(payload, []byte{0xef, 0xbb, 0xbf}) {
		payload = payload[3:]
	}
	parser := workerEnvironmentParser{path: path, payload: payload, line: 1}
	return parser.parse()
}

type workerEnvironmentParser struct {
	path    string
	payload []byte
	offset  int
	line    int
}

func (parser *workerEnvironmentParser) parse() ([]workerEnvironmentEntry, error) {
	entries := make([]workerEnvironmentEntry, 0, 16)
	seen := make(map[string]string, 16)
	for parser.offset < len(parser.payload) {
		parser.skipHorizontalSpace()
		if parser.atLineEnd() {
			if err := parser.consumeLineEnd(); err != nil {
				return nil, err
			}
			continue
		}
		if parser.payload[parser.offset] == '#' {
			parser.skipComment()
			if err := parser.consumeLineEnd(); err != nil {
				return nil, err
			}
			continue
		}

		startLine := parser.line
		if parser.hasExportPrefix() {
			parser.offset += len("export")
			parser.skipHorizontalSpace()
		}
		nameStart := parser.offset
		if parser.offset >= len(parser.payload) || !isWorkerEnvironmentKeyStart(parser.payload[parser.offset]) {
			return nil, parser.lineError(startLine, "invalid variable name")
		}
		parser.offset++
		for parser.offset < len(parser.payload) && isWorkerEnvironmentKeyContinue(parser.payload[parser.offset]) {
			parser.offset++
		}
		name := string(parser.payload[nameStart:parser.offset])
		parser.skipHorizontalSpace()
		if parser.offset >= len(parser.payload) || parser.payload[parser.offset] != '=' {
			return nil, parser.lineError(startLine, "expected '=' after variable name")
		}
		parser.offset++
		hadLeadingWhitespace := parser.offset < len(parser.payload) && isHorizontalSpace(parser.payload[parser.offset])
		parser.skipHorizontalSpace()

		value, err := parser.parseValue(startLine, hadLeadingWhitespace)
		if err != nil {
			return nil, err
		}
		if err := validateWorkerEnvironmentName(name); err != nil {
			return nil, parser.lineError(startLine, err.Error())
		}
		if err := validateWorkerEnvironmentValue(value); err != nil {
			return nil, parser.lineError(startLine, err.Error())
		}
		normalized := normalizeEnvironmentKey(name)
		if previous, exists := seen[normalized]; exists {
			return nil, parser.lineError(startLine, fmt.Sprintf("duplicate variable %q (previously %q)", name, previous))
		}
		seen[normalized] = name
		entries = append(entries, workerEnvironmentEntry{name: name, value: value})
		if len(entries) > maxWorkerEnvironmentCount {
			return nil, parser.lineError(startLine, fmt.Sprintf("environment exceeds %d variables", maxWorkerEnvironmentCount))
		}
	}
	if err := validateWorkerEnvironment(entries); err != nil {
		return nil, fmt.Errorf("worker environment file %q: %w", parser.path, err)
	}
	return entries, nil
}

func (parser *workerEnvironmentParser) parseValue(startLine int, hadLeadingWhitespace bool) (string, error) {
	if parser.offset >= len(parser.payload) || parser.atLineEnd() {
		if err := parser.consumeLineEnd(); err != nil {
			return "", err
		}
		return "", nil
	}
	quote := parser.payload[parser.offset]
	if quote != '\'' && quote != '"' {
		start := parser.offset
		comment := -1
		for parser.offset < len(parser.payload) && !parser.atLineEnd() {
			if parser.payload[parser.offset] == '#' && (parser.offset > start && isHorizontalSpace(parser.payload[parser.offset-1]) || parser.offset == start && hadLeadingWhitespace) {
				comment = parser.offset
				break
			}
			parser.offset++
		}
		end := parser.offset
		if comment >= 0 {
			end = comment
			parser.skipComment()
		}
		value := strings.Trim(string(parser.payload[start:end]), " \t")
		if err := parser.consumeLineEnd(); err != nil {
			return "", err
		}
		return value, nil
	}

	parser.offset++
	value := make([]byte, 0, 64)
	for parser.offset < len(parser.payload) {
		character := parser.payload[parser.offset]
		if character == quote {
			parser.offset++
			parser.skipHorizontalSpace()
			if parser.offset < len(parser.payload) && parser.payload[parser.offset] == '#' {
				parser.skipComment()
			}
			if !parser.atLineEnd() {
				return "", parser.lineError(startLine, "trailing text after quoted value")
			}
			if err := parser.consumeLineEnd(); err != nil {
				return "", err
			}
			return string(value), nil
		}
		if character == '\r' || character == '\n' {
			if err := parser.consumeQuotedLineEnd(&value); err != nil {
				return "", err
			}
			continue
		}
		if quote == '"' && character == '\\' {
			parser.offset++
			if parser.offset >= len(parser.payload) {
				return "", parser.lineError(startLine, "unterminated escape in double-quoted value")
			}
			escaped := parser.payload[parser.offset]
			switch escaped {
			case '\\', '"':
				value = append(value, escaped)
			case 'n':
				value = append(value, '\n')
			case 'r':
				value = append(value, '\r')
			case 't':
				value = append(value, '\t')
			default:
				return "", parser.lineError(startLine, "invalid escape in double-quoted value")
			}
			parser.offset++
			continue
		}
		value = append(value, character)
		parser.offset++
	}
	return "", parser.lineError(startLine, "unterminated quoted value")
}

func (parser *workerEnvironmentParser) hasExportPrefix() bool {
	if !bytes.HasPrefix(parser.payload[parser.offset:], []byte("export")) {
		return false
	}
	next := parser.offset + len("export")
	return next < len(parser.payload) && isHorizontalSpace(parser.payload[next])
}

func (parser *workerEnvironmentParser) skipHorizontalSpace() {
	for parser.offset < len(parser.payload) && isHorizontalSpace(parser.payload[parser.offset]) {
		parser.offset++
	}
}

func (parser *workerEnvironmentParser) skipComment() {
	for parser.offset < len(parser.payload) && !parser.atLineEnd() {
		parser.offset++
	}
}

func (parser *workerEnvironmentParser) atLineEnd() bool {
	return parser.offset >= len(parser.payload) || parser.payload[parser.offset] == '\n' || parser.payload[parser.offset] == '\r'
}

func (parser *workerEnvironmentParser) consumeLineEnd() error {
	if parser.offset >= len(parser.payload) {
		return nil
	}
	if parser.payload[parser.offset] == '\r' {
		if parser.offset+1 >= len(parser.payload) || parser.payload[parser.offset+1] != '\n' {
			return parser.lineError(parser.line, "bare carriage return is not a valid line ending")
		}
		parser.offset += 2
	} else if parser.payload[parser.offset] == '\n' {
		parser.offset++
	} else {
		return parser.lineError(parser.line, "expected end of line")
	}
	parser.line++
	return nil
}

func (parser *workerEnvironmentParser) consumeQuotedLineEnd(value *[]byte) error {
	if err := parser.consumeLineEnd(); err != nil {
		return err
	}
	*value = append(*value, '\n')
	return nil
}

func (parser *workerEnvironmentParser) lineError(line int, message string) error {
	return fmt.Errorf("worker environment file %q line %d: %s", parser.path, line, message)
}

func validateWorkerEnvironment(entries []workerEnvironmentEntry) error {
	if len(entries) > maxWorkerEnvironmentCount {
		return fmt.Errorf("worker environment exceeds %d variables", maxWorkerEnvironmentCount)
	}
	bytesTotal := 1
	unitsTotal := 1
	for _, entry := range entries {
		if err := validateWorkerEnvironmentName(entry.name); err != nil {
			return fmt.Errorf("worker environment variable %q: %w", entry.name, err)
		}
		if err := validateWorkerEnvironmentValue(entry.value); err != nil {
			return fmt.Errorf("worker environment variable %q: %w", entry.name, err)
		}
		bytesTotal += len(entry.name) + 1 + len(entry.value) + 1
		unitsTotal += utf16Units(entry.name) + 1 + utf16Units(entry.value) + 1
	}
	if bytesTotal > maxWorkerEnvironmentUnits || unitsTotal > maxWorkerEnvironmentUnits {
		return fmt.Errorf("worker environment exceeds %d total units", maxWorkerEnvironmentUnits)
	}
	return nil
}

func validateWorkerEnvironmentName(name string) error {
	if err := validateWorkerEnvironmentNameSyntax(name); err != nil {
		return err
	}
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "pogo_worker_") {
		return errors.New("variable name is reserved for worker transport")
	}
	if _, reserved := reservedWorkerEnvironmentNames[lower]; reserved {
		return errors.New("variable name is reserved by Pogo")
	}
	return nil
}

func validateWorkerEnvironmentNameSyntax(name string) error {
	if !utf8.ValidString(name) || name == "" || !isWorkerEnvironmentKeyStart(name[0]) {
		return errors.New("invalid variable name")
	}
	for index := 1; index < len(name); index++ {
		if !isWorkerEnvironmentKeyContinue(name[index]) {
			return errors.New("invalid variable name")
		}
	}
	if len(name) > maxWorkerEnvironmentKeySize || utf16Units(name) > maxWorkerEnvironmentKeySize {
		return fmt.Errorf("variable name exceeds %d units", maxWorkerEnvironmentKeySize)
	}
	return nil
}

func validateWorkerProcessEnvironment(entries []workerEnvironmentEntry) error {
	if len(entries) > maxWorkerEnvironmentCount {
		return fmt.Errorf("worker environment exceeds %d variables", maxWorkerEnvironmentCount)
	}
	seen := make(map[string]string, len(entries))
	bytesTotal := 1
	unitsTotal := 1
	for _, entry := range entries {
		if err := validateWorkerEnvironmentNameSyntax(entry.name); err != nil {
			return fmt.Errorf("worker environment variable %q: %w", entry.name, err)
		}
		if err := validateWorkerEnvironmentValue(entry.value); err != nil {
			return fmt.Errorf("worker environment variable %q: %w", entry.name, err)
		}
		normalized := normalizeEnvironmentKey(entry.name)
		if previous, exists := seen[normalized]; exists {
			return fmt.Errorf("worker environment variables %q and %q refer to the same variable", previous, entry.name)
		}
		seen[normalized] = entry.name
		bytesTotal += len(entry.name) + 1 + len(entry.value) + 1
		unitsTotal += utf16Units(entry.name) + 1 + utf16Units(entry.value) + 1
	}
	if bytesTotal > maxWorkerEnvironmentUnits || unitsTotal > maxWorkerEnvironmentUnits {
		return fmt.Errorf("worker environment exceeds %d total units", maxWorkerEnvironmentUnits)
	}
	return nil
}

func validateWorkerEnvironmentValue(value string) error {
	if !utf8.ValidString(value) {
		return errors.New("value contains invalid UTF-8")
	}
	if strings.IndexByte(value, 0) >= 0 {
		return errors.New("value contains NUL")
	}
	if len(value) > maxWorkerEnvironmentValueSize || utf16Units(value) > maxWorkerEnvironmentValueSize {
		return fmt.Errorf("value exceeds %d units", maxWorkerEnvironmentValueSize)
	}
	return nil
}

func sortWorkerEnvironment(entries []workerEnvironmentEntry) {
	sort.Slice(entries, func(left, right int) bool {
		leftKey := normalizeEnvironmentKey(entries[left].name)
		rightKey := normalizeEnvironmentKey(entries[right].name)
		if leftKey == rightKey {
			return entries[left].name < entries[right].name
		}
		return leftKey < rightKey
	})
}

func utf16Units(value string) int {
	units := 0
	for _, character := range value {
		units++
		if character > 0xffff {
			units++
		}
	}
	return units
}

func isWorkerEnvironmentKeyStart(character byte) bool {
	return character == '_' || character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z'
}

func isWorkerEnvironmentKeyContinue(character byte) bool {
	return isWorkerEnvironmentKeyStart(character) || character >= '0' && character <= '9'
}

func isHorizontalSpace(character byte) bool {
	return character == ' ' || character == '\t'
}

func resolveWorkerEnvironmentFile(projectRoot, path string) (string, error) {
	if path == "" {
		return "", nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(projectRoot, path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve worker environment file: %w", err)
	}
	return filepath.Clean(absolute), nil
}
