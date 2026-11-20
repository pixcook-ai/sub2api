package service

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/managedfiles"
	"github.com/tidwall/gjson"
)

var ErrManagedFileUnsupportedInput = errors.New("unsupported managed file input")

type ManagedFileReference struct {
	PublicID  string
	Positions []managedfiles.Position
}

// CollectManagedResponseFiles reads only typed Responses content. Tool
// arguments and custom input strings remain opaque, even when they contain IDs.
func CollectManagedResponseFiles(body []byte) ([]ManagedFileReference, error) {
	if candidate, alias := managedResponseFileCandidate(body); candidate {
		if alias {
			return nil, ErrManagedFileUnsupportedInput
		}
		if _, err := managedfiles.Parse(body); err != nil {
			return nil, ErrManagedFileUnsupportedInput
		}
	}
	refs := make(map[string][]managedfiles.Position)
	unsupported := false
	hasFile := false
	input := gjson.GetBytes(body, "input")
	if input.IsObject() {
		for _, part := range input.Get("content").Array() {
			if part.Get("type").String() == "input_file" || (part.Get("type").String() == "input_image" && part.Get("file_id").Exists()) {
				return nil, ErrManagedFileUnsupportedInput
			}
		}
	}
	for i, item := range input.Array() {
		kind := item.Get("type").String()
		if kind == "input_file" || (kind == "input_image" && item.Get("file_id").Exists()) {
			return nil, ErrManagedFileUnsupportedInput
		}
		if kind == "function_call_output" || kind == "custom_tool_call_output" {
			for _, part := range item.Get("output").Array() {
				if found, _ := managedFilePartCandidate(part, 0); found {
					return nil, ErrManagedFileUnsupportedInput
				}
			}
			continue
		}
		if kind != "message" && !(kind == "" && item.Get("role").Type == gjson.String) {
			continue
		}
		for j, part := range item.Get("content").Array() {
			switch part.Get("type").String() {
			case "input_image":
				if part.Get("file_id").Exists() {
					return nil, ErrManagedFileUnsupportedInput
				}
			case "input_file":
				id := part.Get("file_id")
				if id.Exists() {
					hasFile = true
					if id.Type != gjson.String || !managedfiles.ValidRemoteID(id.String()) {
						return nil, fmt.Errorf("%w: invalid file reference", ErrManagedFileUnsupportedInput)
					}
					refs[id.String()] = append(refs[id.String()], managedfiles.Position{Input: i, Part: j})
				}
				if part.Get("file_data").Exists() || part.Get("file_url").Exists() {
					unsupported = true
				}
			}
		}
	}
	if !hasFile {
		return nil, nil
	}
	if unsupported {
		return nil, ErrManagedFileUnsupportedInput
	}
	if _, err := managedfiles.Parse(body); err != nil {
		return nil, fmt.Errorf("%w: invalid managed JSON", ErrManagedFileUnsupportedInput)
	}
	ids := make([]string, 0, len(refs))
	for id := range refs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]ManagedFileReference, 0, len(ids))
	for _, id := range ids {
		result = append(result, ManagedFileReference{PublicID: id, Positions: refs[id]})
	}
	return result, nil
}

// Walk only protocol-owned input/content/output containers, including duplicate
// and case aliases. Opaque tool arguments and user text never participate.
func managedResponseFileCandidate(body []byte) (bool, bool) {
	found, alias := false, false
	scanPart := func(part gjson.Result) {
		partFound, partAlias := managedFilePartCandidate(part, 0)
		found, alias = found || partFound, alias || partAlias
	}
	scanItem := func(item gjson.Result) {
		scanPart(item)
		item.ForEach(func(k, v gjson.Result) bool {
			if strings.EqualFold(k.String(), "content") || strings.EqualFold(k.String(), "output") {
				alias = alias || (k.String() != "content" && k.String() != "output")
				if v.IsObject() {
					scanPart(v)
				} else {
					v.ForEach(func(_, p gjson.Result) bool { scanPart(p); return !found })
				}
			}
			return !found
		})
	}
	gjson.ParseBytes(body).ForEach(func(k, v gjson.Result) bool {
		if strings.EqualFold(k.String(), "input") {
			alias = alias || k.String() != "input"
			if v.IsObject() {
				scanItem(v)
			} else {
				v.ForEach(func(_, item gjson.Result) bool { scanItem(item); return !found })
			}
		}
		return !found
	})
	return found, alias
}

// Only typed tool-result content is traversed. Tool arguments and arbitrary
// objects remain opaque; duplicate type keys are all considered before Parse
// rejects ambiguous JSON.
func managedFilePartCandidate(part gjson.Result, depth int) (found, alias bool) {
	if !part.IsObject() {
		return false, false
	}
	if depth > 64 {
		return true, true
	}
	fileKind, toolResult, hasID := false, false, false
	part.ForEach(func(k, v gjson.Result) bool {
		switch {
		case strings.EqualFold(k.String(), "type"):
			fileKind = fileKind || v.String() == "input_file" || v.String() == "input_image"
			toolResult = toolResult || v.String() == "tool_result"
			alias = alias || k.String() != "type"
		case strings.EqualFold(k.String(), "file_id"):
			hasID = true
			alias = alias || k.String() != "file_id"
		}
		return true
	})
	if fileKind && hasID {
		return true, alias
	}
	if toolResult {
		part.ForEach(func(k, v gjson.Result) bool {
			if strings.EqualFold(k.String(), "content") {
				alias = alias || k.String() != "content"
				v.ForEach(func(_, child gjson.Result) bool {
					childFound, childAlias := managedFilePartCandidate(child, depth+1)
					found, alias = found || childFound, alias || childAlias
					return true
				})
			}
			return true
		})
	}
	return found, alias
}
