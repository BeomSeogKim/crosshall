package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
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
		// The group lists one handler per event; the entry's event names
		// the handler that records the installed executable.
		if entry.Group != agyHookGroup || !eventName.MatchString(entry.Event) || node.kind != '{' || len(node.fields) == 0 {
			return configEntry{}, errInvalidEntry
		}
		for _, field := range node.fields {
			list := field.value
			if !eventName.MatchString(field.key) || list == nil || list.kind != '[' || len(list.items) != 1 || list.items[0].kind != '{' {
				return configEntry{}, errInvalidEntry
			}
		}
		list := node.field(entry.Event)
		if list == nil {
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
	slot := slotPath(entry)
	section, key := slot[0], slot[1]
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

// slotPath names the object and the list that hold an entry.
func slotPath(entry Entry) []string {
	switch {
	case entry.AutoMode:
		return []string{"autoMode", "allow"}
	case entry.Event == "":
		return []string{"permissions", "allow"}
	}
	return []string{"hooks", entry.Event}
}

// createdSlots lists the objects and lists holding the entries that exist
// after an edit but not before it: setup created them for its entries.
func createdSlots(before, after []byte, entries []Entry) ([][]string, error) {
	was, err := configDocument(before)
	if err != nil {
		return nil, err
	}
	now, err := configDocument(after)
	if err != nil {
		return nil, err
	}
	var created [][]string
	for _, entry := range entries {
		if entry.Group != "" {
			continue
		}
		path := slotPath(entry)
		for n := 1; n <= len(path); n++ {
			if configAt(now, path[:n]) != nil && configAt(was, path[:n]) == nil {
				created = appendSlots(created, [][]string{path[:n]})
			}
		}
	}
	return created, nil
}

func appendSlots(slots, more [][]string) [][]string {
	for _, slot := range more {
		if !slices.ContainsFunc(slots, func(s []string) bool { return slices.Equal(s, slot) }) {
			slots = append(slots, slices.Clone(slot))
		}
	}
	return slots
}

func configAt(doc *configNode, path []string) *configNode {
	for _, key := range path {
		if doc == nil || doc.kind != '{' {
			return nil
		}
		doc = doc.field(key)
	}
	return doc
}

// pruneSlots removes each listed object or list that is empty, deepest
// first, so an object emptied by removing its last list goes too.
func pruneSlots(raw []byte, slots [][]string) ([]byte, error) {
	ordered := slices.Clone(slots)
	slices.SortStableFunc(ordered, func(a, b []string) int { return len(b) - len(a) })
	for _, path := range ordered {
		doc, err := configDocument(raw)
		if err != nil || doc == nil {
			return raw, err
		}
		parent := configAt(doc, path[:len(path)-1])
		if parent == nil || parent.kind != '{' {
			continue
		}
		i := slices.IndexFunc(parent.fields, func(f configField) bool { return f.key == path[len(path)-1] })
		if i < 0 {
			continue
		}
		if value := parent.fields[i].value; value.kind == '[' && len(value.items) == 0 || value.kind == '{' && len(value.fields) == 0 {
			start, end := configEntryRange(parent, i)
			raw = spliceConfig(raw, start, end, nil)
		}
	}
	return raw, nil
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

func removeEntries(original []byte, claims []Entry) (updated []byte, removed, remaining []Entry, err error) {
	updated = original
	for _, entry := range claims {
		doc, err := configDocument(updated)
		if err != nil {
			return nil, nil, nil, err
		}
		list, _, matches, err := locateConfigEntry(doc, entry)
		if err != nil {
			return nil, nil, nil, err
		}
		// A string claim has no identity beyond its exact value.
		if entry.Event == "" && matches.full == 0 {
			continue
		}
		if matches.full != 1 || matches.related != 1 {
			remaining = append(remaining, entry)
			continue
		}
		start, end := configEntryRange(list, matches.index)
		updated = spliceConfig(updated, start, end, nil)
		removed = append(removed, entry)
	}
	return updated, removed, remaining, nil
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
