// SPDX-License-Identifier: Apache-2.0

package comfyui

// Reference is one resolved file reference — a loader node naming a real
// (or once-real) model file, or an embedding token found in a text widget.
type Reference struct {
	FolderType string
	Name       string
}

// RefSet accumulates references (union across every workflow/queue/history
// source BuildMap reads) plus every class_type actually seen — the raw
// material for guardrail (c) (an unknown class_type touching a model
// folder). Recording "every class_type seen" here, not just the ones this
// package recognizes, is what lets BuildMap notice a loader kind it has no
// LoaderSpec for at all, not only a known one used unexpectedly.
type RefSet struct {
	Refs           map[Reference]bool
	ClassTypesSeen map[string]bool
}

// NewRefSet returns an empty, ready-to-use RefSet.
func NewRefSet() *RefSet {
	return &RefSet{Refs: map[Reference]bool{}, ClassTypesSeen: map[string]bool{}}
}

// Add records one reference.
func (s *RefSet) Add(folderType, name string) {
	if name == "" {
		return
	}
	s.Refs[Reference{FolderType: folderType, Name: name}] = true
}

// SeeClass records that classType was encountered, known or not.
func (s *RefSet) SeeClass(classType string) {
	if classType != "" {
		s.ClassTypesSeen[classType] = true
	}
}

// Has reports whether (folderType, name) was referenced anywhere.
func (s *RefSet) Has(folderType, name string) bool {
	return s.Refs[Reference{FolderType: folderType, Name: name}]
}
