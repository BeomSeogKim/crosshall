package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

type Entry struct {
	Event    string          `json:"event,omitempty"`
	Group    string          `json:"group,omitempty"`
	AutoMode bool            `json:"auto_mode,omitempty"`
	Value    json.RawMessage `json:"value"`
}

const agyHookGroup = "crosshall"

var (
	errInvalidConfig = errors.New("invalid or ambiguous vendor configuration JSON")
	errInvalidEntry  = errors.New("invalid setup entry")
	errAmbiguousHook = errors.New("a conflicting hook already uses the same command and arguments")
)

// configNode tracks byte ranges so edits leave unrelated values and key order intact.
type configNode struct {
	start, end int
	kind       byte
	atom       any
	fields     []configField
	items      []*configNode
}

type configField struct {
	key   string
	start int
	value *configNode
}

func parseConfigJSON(raw []byte) (*configNode, error) {
	if !utf8.Valid(raw) {
		return nil, errInvalidConfig
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	node, err := readConfigNode(raw, dec)
	if err != nil {
		return nil, errInvalidConfig
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errInvalidConfig
	}
	return node, nil
}

func readConfigNode(raw []byte, dec *json.Decoder) (*configNode, error) {
	start := int(dec.InputOffset())
	for start < len(raw) && strings.IndexByte(" \r\n\t,:", raw[start]) >= 0 {
		start++
	}
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	node := &configNode{start: start, atom: token}
	switch token {
	case json.Delim('{'):
		node.kind = '{'
		seen := make(map[string]bool)
		for dec.More() {
			keyStart := int(dec.InputOffset())
			for keyStart < len(raw) && strings.IndexByte(" \r\n\t,", raw[keyStart]) >= 0 {
				keyStart++
			}
			token, err := dec.Token()
			key, ok := token.(string)
			if err != nil || !ok || seen[key] {
				return nil, errInvalidConfig
			}
			seen[key] = true
			child, err := readConfigNode(raw, dec)
			if err != nil {
				return nil, err
			}
			node.fields = append(node.fields, configField{key, keyStart, child})
		}
	case json.Delim('['):
		node.kind = '['
		for dec.More() {
			child, err := readConfigNode(raw, dec)
			if err != nil {
				return nil, err
			}
			node.items = append(node.items, child)
		}
	}
	if node.kind != 0 {
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
	}
	node.end = int(dec.InputOffset())
	return node, nil
}

func (n *configNode) field(key string) *configNode {
	for _, field := range n.fields {
		if field.key == key {
			return field.value
		}
	}
	return nil
}

func (n *configNode) stringValue() (string, bool) {
	if n == nil {
		return "", false
	}
	value, ok := n.atom.(string)
	return value, ok
}

func sameConfigValue(a, b *configNode) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case '{':
		return sameConfigFields(a, b, false)
	case '[':
		if len(a.items) != len(b.items) {
			return false
		}
		for i := range a.items {
			if !sameConfigValue(a.items[i], b.items[i]) {
				return false
			}
		}
		return true
	default:
		return a.atom == b.atom
	}
}

func sameConfigFields(a, b *configNode, ignoreHooks bool) bool {
	if len(a.fields) != len(b.fields) {
		return false
	}
	for _, field := range a.fields {
		if ignoreHooks && field.key == "hooks" {
			continue
		}
		if !sameConfigValue(field.value, b.field(field.key)) {
			return false
		}
	}
	return true
}

type configEntry struct {
	value   *configNode
	handler *configNode
}

func parseConfigEntry(entry Entry) (configEntry, error) {
	if entry.AutoMode && (entry.Event != "" || entry.Group != "") {
		return configEntry{}, errInvalidEntry
	}
	node, err := parseConfigJSON(entry.Value)
	if err != nil {
		return configEntry{}, errInvalidEntry
	}
	parsed := configEntry{value: node}
	switch {
	case entry.Group != "":
		if entry.Group != agyHookGroup || entry.Event != "SessionStart" || node.kind != '{' || len(node.fields) != 1 {
			return configEntry{}, errInvalidEntry
		}
		list := node.field(entry.Event)
		if list == nil || list.kind != '[' || len(list.items) != 1 || list.items[0].kind != '{' {
			return configEntry{}, errInvalidEntry
		}
		parsed.handler = list.items[0]
	case entry.Event == "":
		if _, ok := node.stringValue(); !ok {
			return configEntry{}, errInvalidEntry
		}
		return parsed, nil
	default:
		children, err := configHookHandlers(node)
		if err != nil || len(children) != 1 {
			return configEntry{}, errInvalidEntry
		}
		parsed.handler = children[0]
	}
	typ, _ := parsed.handler.field("type").stringValue()
	command, _ := parsed.handler.field("command").stringValue()
	if typ != "command" || command == "" {
		return configEntry{}, errInvalidEntry
	}
	return parsed, nil
}

func configHookHandlers(group *configNode) ([]*configNode, error) {
	if group.kind != '{' {
		return nil, errInvalidConfig
	}
	list := group.field("hooks")
	if list == nil || list.kind != '[' {
		return nil, errInvalidConfig
	}
	for _, handler := range list.items {
		if handler.kind != '{' {
			return nil, errInvalidConfig
		}
	}
	return list.items, nil
}

// An entry slot is the existing array or the object and missing keys needed to create it.
func findConfigSlot(doc *configNode, entry Entry) (container *configNode, missing []string, err error) {
	if doc.kind != '{' {
		return nil, nil, errInvalidConfig
	}
	section, key := "hooks", entry.Event
	if entry.AutoMode {
		section, key = "autoMode", "allow"
	} else if entry.Event == "" {
		section, key = "permissions", "allow"
	}
	object := doc.field(section)
	if object == nil {
		return doc, []string{section, key}, nil
	}
	if object.kind != '{' {
		return nil, nil, errInvalidConfig
	}
	list := object.field(key)
	if list == nil {
		return object, []string{key}, nil
	}
	if list.kind != '[' {
		return nil, nil, errInvalidConfig
	}
	return list, nil, nil
}

type configMatches struct {
	installed, related, full int
	index                    int
}

func matchConfigEntry(list *configNode, want configEntry) (configMatches, error) {
	var matches configMatches
	for index, node := range list.items {
		if want.handler == nil {
			if _, ok := node.stringValue(); !ok {
				return configMatches{}, errInvalidConfig
			}
			if sameConfigValue(node, want.value) {
				matches.installed++
				matches.related++
				matches.full++
				matches.index = index
			}
			continue
		}
		handlers, err := configHookHandlers(node)
		if err != nil {
			return configMatches{}, err
		}
		for _, handler := range handlers {
			if sameConfigValue(handler.field("command"), want.handler.field("command")) &&
				sameConfigValue(handler.field("args"), want.handler.field("args")) {
				matches.related++
				if sameConfigFields(node, want.value, true) && sameConfigValue(handler, want.handler) {
					matches.installed++
				}
			}
		}
		if sameConfigValue(node, want.value) {
			matches.full++
			matches.index = index
		}
	}
	return matches, nil
}

func addEntries(original []byte, want []Entry) ([]byte, []Entry, error) {
	updated := original
	var added []Entry
	for _, entry := range want {
		if len(updated) == 0 {
			updated = []byte("{}\n")
		}
		doc, err := parseConfigJSON(updated)
		if err != nil {
			return nil, nil, err
		}
		container, missing, matches, err := locateConfigEntry(doc, entry)
		if err != nil {
			return nil, nil, err
		}
		if entry.Event != "" && matches.related > 0 && (matches.related != 1 || matches.installed != 1) {
			return nil, nil, errAmbiguousHook
		}
		if matches.installed > 0 {
			continue
		}
		if entry.Group != "" {
			key, _ := json.Marshal(entry.Group)
			updated = appendConfigValue(updated, container, append(append(key, ':'), entry.Value...))
		} else {
			updated = appendConfigEntry(updated, container, missing, entry.Value)
		}
		added = append(added, entry)
	}
	return updated, added, nil
}

func removeEntries(original []byte, claims []Entry) ([]byte, []Entry, error) {
	updated := original
	var remaining []Entry
	for _, entry := range claims {
		doc, err := configDocument(updated)
		if err != nil {
			return nil, nil, err
		}
		list, _, matches, err := locateConfigEntry(doc, entry)
		if err != nil {
			return nil, nil, err
		}
		if matches.full != 1 || matches.related != 1 {
			remaining = append(remaining, entry)
			continue
		}
		start, end := configEntryRange(list, matches.index)
		updated = spliceConfig(updated, start, end, nil)
	}
	return updated, remaining, nil
}

func entryMatches(original []byte, entries []Entry) ([]int, error) {
	doc, err := configDocument(original)
	if err != nil {
		return nil, err
	}
	counts := make([]int, len(entries))
	for i, entry := range entries {
		_, _, matches, err := locateConfigEntry(doc, entry)
		if err != nil {
			return nil, err
		}
		counts[i] = matches.installed
	}
	return counts, nil
}

func configDocument(raw []byte) (*configNode, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	return parseConfigJSON(raw)
}

func locateConfigEntry(doc *configNode, entry Entry) (*configNode, []string, configMatches, error) {
	want, err := parseConfigEntry(entry)
	if err != nil || doc == nil {
		return nil, nil, configMatches{}, err
	}
	if entry.Group != "" {
		if doc.kind != '{' {
			return nil, nil, configMatches{}, errInvalidConfig
		}
		for i, field := range doc.fields {
			if field.key == entry.Group {
				matches := configMatches{related: 1, index: i}
				if sameConfigValue(field.value, want.value) {
					matches.installed, matches.full = 1, 1
				}
				return doc, nil, matches, nil
			}
		}
		return doc, nil, configMatches{}, nil
	}
	container, missing, err := findConfigSlot(doc, entry)
	if err != nil || len(missing) > 0 {
		return container, missing, configMatches{}, err
	}
	matches, err := matchConfigEntry(container, want)
	return container, missing, matches, err
}

func appendConfigEntry(raw []byte, container *configNode, missing []string, value []byte) []byte {
	if len(missing) > 0 {
		value = append(append([]byte{'['}, value...), ']')
		for i := len(missing) - 1; i >= 0; i-- {
			key, _ := json.Marshal(missing[i])
			value = append(append(key, ':'), value...)
			if i > 0 {
				value = append(append([]byte{'{'}, value...), '}')
			}
		}
	}
	return appendConfigValue(raw, container, value)
}

func appendConfigValue(raw []byte, container *configNode, value []byte) []byte {
	offset := container.start + 1
	if len(container.fields) > 0 {
		offset = container.fields[len(container.fields)-1].value.end
	} else if len(container.items) > 0 {
		offset = container.items[len(container.items)-1].end
	}
	if offset != container.start+1 {
		value = append([]byte{','}, value...)
	}
	return spliceConfig(raw, offset, offset, value)
}

func configEntryRange(container *configNode, index int) (start, end int) {
	if container.kind == '{' {
		field := container.fields[index]
		start, end = field.start, field.value.end
		if index > 0 {
			start = container.fields[index-1].value.end
		} else if len(container.fields) > 1 {
			end = container.fields[1].start
		}
		return start, end
	}
	start, end = container.items[index].start, container.items[index].end
	if index > 0 {
		start = container.items[index-1].end
	} else if len(container.items) > 1 {
		end = container.items[1].start
	}
	return start, end
}

func spliceConfig(raw []byte, start, end int, value []byte) []byte {
	updated := make([]byte, 0, start+len(value)+len(raw)-end)
	updated = append(updated, raw[:start]...)
	updated = append(updated, value...)
	return append(updated, raw[end:]...)
}
