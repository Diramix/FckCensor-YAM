package replacements

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

const Source = "https://raw.githubusercontent.com/Hazzz895/FckCensorData/main/list_v2.json"

var Numeric = regexp.MustCompile(`^[0-9]{1,20}$`)
var Stream = regexp.MustCompile(`\.[0-9]+\.([0-9]+)/(?:(?:aac|mp3|opus|flac)[^/? ]*)`)

type storage struct {
	Template string                     `json:"urlTemplate"`
	IDs      []json.RawMessage          `json:"trackIds"`
	Tracks   map[string]json.RawMessage `json:"tracks"`
}

func safeURL(value string) (string, error) {
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host != "raw.githubusercontent.com" || u.User != nil || u.Fragment != "" {
		return "", fmt.Errorf("unsupported replacement URL")
	}
	return u.String(), nil
}

func Build(data []byte) (map[string]string, error) {
	var doc struct {
		Sources []struct {
			Storages []storage `json:"tracksStorages"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Sources) == 0 {
		return nil, fmt.Errorf("replacement list has no sources")
	}
	index := map[string]string{}
	put := func(id, value string) error {
		if !Numeric.MatchString(id) {
			return fmt.Errorf("invalid track ID")
		}
		if _, exists := index[id]; exists {
			return nil
		}
		u, err := safeURL(value)
		if err != nil {
			return err
		}
		index[id] = u
		return nil
	}
	for _, source := range doc.Sources {
		for _, store := range source.Storages {
			for _, raw := range store.IDs {
				var object struct {
					ID json.Number `json:"id"`
				}
				var id json.Number
				if len(raw) > 0 && raw[0] == '{' {
					if err := json.Unmarshal(raw, &object); err != nil {
						return nil, err
					}
					id = object.ID
				} else {
					if err := json.Unmarshal(raw, &id); err != nil {
						return nil, err
					}
				}
				if store.Template != "" {
					if err := put(id.String(), strings.ReplaceAll(store.Template, "%%", id.String())); err != nil {
						return nil, err
					}
				}
			}
			for id, raw := range store.Tracks {
				var value string
				if len(raw) > 0 && raw[0] == '{' {
					var obj struct {
						URL string `json:"url"`
					}
					if err := json.Unmarshal(raw, &obj); err != nil {
						return nil, err
					}
					value = obj.URL
				} else if len(raw) > 0 && raw[0] == '"' {
					if err := json.Unmarshal(raw, &value); err != nil {
						return nil, err
					}
				} else {
					var number json.Number
					if err := json.Unmarshal(raw, &number); err != nil {
						return nil, err
					}
					value = strings.ReplaceAll(store.Template, "%%", number.String())
				}
				if err := put(id, value); err != nil {
					return nil, err
				}
			}
		}
	}
	return index, nil
}
