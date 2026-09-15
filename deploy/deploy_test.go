package deploy

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/d13co/algod-loadb-mesh/internal/config"
)

// The examples must keep up with the config schema: every key known, and
// valid once the placeholders a real host fills in are set.
func TestExamplesLoad(t *testing.T) {
	for name, text := range map[string]string{"config.example.yaml": ConfigExample, "config.static.example.yaml": ConfigStaticExample} {
		c, err := config.Decode([]byte(text))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		c.ClientToken = "t" // instead of reading client_token_file
		c.AdminToken = "a"  // instead of reading algod.admin.token from data_dir
		if c.Registry.SyncKeyFile != "" {
			c.Registry.SyncKey = "00000000000000000000000000000000000000000000000000000000000000ff"
		}
		if c.Registry.Type == "algorand" && c.Registry.AppID == 0 {
			c.Registry.AppID = 1
		}
		if err := c.Finish(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// commentedOption matches a commented-out `key: value` or `- key: value` line.
var commentedOption = regexp.MustCompile(`(?m)^(\s*)# (\s*(?:- )?[a-z_]+:.*)$`)

// The full example documents every option, and its commented options are
// real keys with values of the right type.
func TestExampleListsEveryOption(t *testing.T) {
	all := commentedOption.ReplaceAllString(ConfigExample, "$1$2")
	if _, err := config.Decode([]byte(all)); err != nil {
		t.Fatalf("example with every option uncommented: %v", err)
	}
	// Registry bookkeeping, not something a static entry sets.
	skip := map[string]bool{"version": true, "updatedat": true}
	for _, key := range yamlKeys(reflect.TypeOf(config.Config{})) {
		if !skip[key] && !regexp.MustCompile(`[\s{,]`+key+`:`).MatchString(all) {
			t.Errorf("option %q missing from config.example.yaml", key)
		}
	}
}

func yamlKeys(t reflect.Type) []string {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map:
		return yamlKeys(t.Elem())
	case reflect.Struct:
	default:
		return nil
	}
	var keys []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "" {
			name = strings.ToLower(f.Name) // yaml.v3's default
		}
		keys = append(keys, name)
		keys = append(keys, yamlKeys(f.Type)...)
	}
	return keys
}
