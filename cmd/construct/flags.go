package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pkar/construct/internal/image"
)

// layerFlags collects -layer, -add, -mkdir, and -symlink in command-line
// order. Items go into the layer started by the latest -layer, or into an
// unnamed first layer if no -layer came before them.
type layerFlags struct{ layers []image.LayerSpec }

func (l *layerFlags) addItem(it image.Item) {
	if len(l.layers) == 0 {
		l.layers = append(l.layers, image.LayerSpec{})
	}
	last := &l.layers[len(l.layers)-1]
	last.Items = append(last.Items, it)
}

type layerFlag struct{ l *layerFlags }

func (f layerFlag) String() string { return "" }

func (f layerFlag) Set(name string) error {
	if name == "" {
		return fmt.Errorf("want a layer name")
	}
	for _, ls := range f.l.layers {
		if ls.Name == name {
			return fmt.Errorf("layer %q given twice", name)
		}
	}
	f.l.layers = append(f.l.layers, image.LayerSpec{Name: name})
	return nil
}

type itemFlag struct {
	l     *layerFlags
	parse func(string) (image.Item, error)
}

func (f itemFlag) String() string { return "" }

func (f itemFlag) Set(v string) error {
	it, err := f.parse(v)
	if err != nil {
		return err
	}
	f.l.addItem(it)
	return nil
}

// repeated collects every value of a repeatable flag.
type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, ",") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

// optionalList is a command line given either as a JSON array or as
// space-separated words. It stays nil when the flag is not set, so the base
// image value is inherited; -entrypoint '[]' clears it.
type optionalList struct{ list []string }

func (o *optionalList) String() string {
	if o == nil || o.list == nil {
		return ""
	}
	b, _ := json.Marshal(o.list)
	return string(b)
}

func (o *optionalList) Set(v string) error {
	if strings.HasPrefix(strings.TrimSpace(v), "[") {
		var list []string
		if err := json.Unmarshal([]byte(v), &list); err != nil {
			return fmt.Errorf("want a JSON array of strings: %w", err)
		}
		if list == nil {
			list = []string{}
		}
		o.list = list
		return nil
	}
	o.list = strings.Fields(v)
	return nil
}
