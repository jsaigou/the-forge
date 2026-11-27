// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"
)

type fakeCatalogAPI struct {
	builds string
	posted []map[string]any
}

func (f *fakeCatalogAPI) GetJSON(path string, v any) error {
	body := f.builds
	if path == "/api/v1/catalog/engines" {
		body = `[{"id":1,"kind":"llamacpp"},{"id":3,"kind":"radiance"}]`
	}
	return json.Unmarshal([]byte(body), v)
}

func (f *fakeCatalogAPI) PostJSON(_ string, body, v any) error {
	f.posted = append(f.posted, body.(map[string]any))
	return json.Unmarshal([]byte(`{"id":7}`), v)
}

func TestRegisterLlamaBuild(t *testing.T) {
	const bin = "/opt/forge/llama.cpp/build-vulkan/bin/llama-server"
	api := &fakeCatalogAPI{builds: `[]`}
	out, err := registerLlamaBuild(api, "llama.cpp (vulkan)", bin, "vulkan")
	if err != nil || !out.Created || out.ID != 7 || len(api.posted) != 1 || api.posted[0]["engine_id"] != int64(1) || api.posted[0]["binary_path"] != bin {
		t.Fatalf("new: %+v %v %v", out, err, api.posted)
	}
	// The same program under whatever name is already there: nothing is added.
	api = &fakeCatalogAPI{builds: `[{"id":4,"engine_id":1,"name":"mine","binary_path":"` + bin + `","backend":"vulkan"}]`}
	if out, err = registerLlamaBuild(api, "llama.cpp (vulkan)", bin, "vulkan"); err != nil || out.Created || out.ID != 4 || len(api.posted) != 0 {
		t.Fatalf("registered already: %+v %v", out, err)
	}
	// A retired build does not count; its name, still taken, is reported rather than reused for another program.
	api = &fakeCatalogAPI{builds: `[{"id":4,"engine_id":1,"name":"old","binary_path":"` + bin + `","backend":"vulkan","retired":true}]`}
	if out, err = registerLlamaBuild(api, "llama.cpp (vulkan)", bin, "vulkan"); err != nil || !out.Created {
		t.Fatalf("after retiring: %+v %v", out, err)
	}
	api = &fakeCatalogAPI{builds: `[{"id":5,"engine_id":1,"name":"llama.cpp (vulkan)","binary_path":"/opt/forge/other","backend":"vulkan"}]`}
	if _, err = registerLlamaBuild(api, "llama.cpp (vulkan)", bin, "vulkan"); err == nil || len(api.posted) != 0 {
		t.Fatalf("name taken: %v %v", err, api.posted)
	}
}
