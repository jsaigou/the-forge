// SPDX-License-Identifier: Apache-2.0

package engine

import "testing"

// execStartPath decodes the Service interface's ExecStart property
// (D-Bus signature a(sasbttttuii)) as godbus hands it back: []interface{}
// of struct-shaped []interface{} entries, field 0 = path. These cases
// pin the real shape plus every malformed input a bus hiccup or a
// non-service unit type could hand back — it must degrade to "", never
// panic (a panic here would kill foundryd, not just one check).
func TestExecStartPath(t *testing.T) {
	cases := []struct {
		name string
		raw  interface{}
		want string
	}{
		{
			name: "real shape, one ExecStart entry",
			raw: []interface{}{
				[]interface{}{
					"/usr/local/lib/forge/start-comfyui.sh",
					[]interface{}{"/usr/local/lib/forge/start-comfyui.sh"},
					false, uint64(0), uint64(0), uint64(0), uint64(0), uint32(0), int32(0),
				},
			},
			want: "/usr/local/lib/forge/start-comfyui.sh",
		},
		{
			name: "multiple ExecStart= lines — reads the first",
			raw: []interface{}{
				[]interface{}{"/bin/first", []interface{}{}, false, uint64(0), uint64(0), uint64(0), uint64(0), uint32(0), int32(0)},
				[]interface{}{"/bin/second", []interface{}{}, false, uint64(0), uint64(0), uint64(0), uint64(0), uint32(0), int32(0)},
			},
			want: "/bin/first",
		},
		{
			// A concretely-typed [][]interface{} outer slice — an
			// alternate shape godbus's generic decode can produce instead
			// of []interface{} of []interface{}, depending on how it
			// resolves the STRUCT array's element type. Both must work:
			// the real production shape wasn't directly observable without
			// a live deploy, so this is defended against, not assumed away.
			name: "concretely-typed [][]interface{} outer slice",
			raw: [][]interface{}{
				{"/usr/local/lib/forge/start-comfyui.sh", []interface{}{"/usr/local/lib/forge/start-comfyui.sh"}, false, uint64(0), uint64(0), uint64(0), uint64(0), uint32(814953), int32(0)},
			},
			want: "/usr/local/lib/forge/start-comfyui.sh",
		},
		{name: "nil (property absent)", raw: nil, want: ""},
		{name: "empty array (non-service unit type)", raw: []interface{}{}, want: ""},
		{name: "wrong outer type", raw: "not an array", want: ""},
		{name: "wrong inner type", raw: []interface{}{"not a struct"}, want: ""},
		{name: "empty struct", raw: []interface{}{[]interface{}{}}, want: ""},
		{name: "field 0 not a string", raw: []interface{}{[]interface{}{uint32(1)}}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := execStartPath(tc.raw); got != tc.want {
				t.Errorf("execStartPath(%#v) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
