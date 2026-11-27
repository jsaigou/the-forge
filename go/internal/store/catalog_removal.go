// SPDX-License-Identifier: Apache-2.0

package store

// Removing a Config, or a whole Model, together with what hangs off it. Deleting a model used to mean deleting each config by hand, then each
// variant, then the model; benchmarks and notes (which have no foreign key) stayed behind, and the weights stayed on disk. This module does it in
// two steps with one set of rules: Plan says exactly what would go and what stands in the way, Execute carries it out.
//
// Rules:
//   - Nothing is removed while something still depends on a Config being removed: a slot running or loading it, a pool engine, a scheduled job, a
//     reservation that has not ended, a queued load, a setting that names it, or another Config using one of the files' catalog rows.
//   - Files are deleted only when asked for, only under the models directory (symlinked parents resolved), and only when nothing else refers to
//     them: no other Artifact, Config argument, pool engine or setting. Symlinks are left alone.
//   - Deleting files requires the Token of the Plan the operator saw, so a catalog that changed in between cannot widen what is deleted.
//   - Rows go first, in one transaction; files after. A file that cannot be deleted is reported, never ignored.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Removal kinds.
const (
	RemoveKindModel  = "model"
	RemoveKindConfig = "config"
)

// Blocker codes: why a removal cannot go ahead.
const (
	BlockConfigLoaded   = "config_loaded"
	BlockPoolEngine     = "pool_engine"
	BlockScheduledJob   = "scheduled_job"
	BlockReservation    = "reservation"
	BlockQueuedLoad     = "queued_load"
	BlockSetting        = "setting"
	BlockSharedArtifact = "shared_artifact"
)

// Reasons a file is kept although its catalog row goes (or would go).
const (
	KeepShared     = "shared"             // another Artifact is the same file, or overlaps it
	KeepReferenced = "referenced"         // a Config argument, pool engine or setting names it
	KeepOutside    = "outside_models_dir" // not inside the models directory
	KeepSymlink    = "symlink"            // a link: what it points at is not this catalog's to delete
	KeepMissing    = "missing"            // already gone
)

var (
	// ErrRemovalBlocked: the plan has blockers.
	ErrRemovalBlocked = errors.New("store: removal is blocked")
	// ErrRemovalChanged: the catalog or the disk changed since the plan the caller confirmed.
	ErrRemovalChanged = errors.New("store: removal plan changed")
)

// RemovalBlocker is one thing standing in the way. Config is the Config concerned, Name the thing that depends on it.
type RemovalBlocker struct {
	Code   string
	Config string
	Name   string
}

// RemovalConfig names a Config that would be removed.
type RemovalConfig struct {
	ID   int64
	Name string
}

// RemovalFile is one file (or directory) behind the Artifacts concerned.
type RemovalFile struct {
	Path      string // absolute
	Bytes     int64
	IsDir     bool
	Delete    bool   // would be deleted if the operator asks for files
	Keep      string // why not, when Delete is false
	KeptBy    string // what refers to it, for KeepShared and KeepReferenced
	Artifacts []int64
}

// RemovalPlan is what a removal would do.
type RemovalPlan struct {
	Kind      string
	ID        int64
	Name      string
	ModelID   int64
	ModelName string
	Configs   []RemovalConfig
	Variants  int
	Offerings []string // provider names of the remote offerings that go with a model
	Aliases   []string // model aliases that go with the configs
	// Benchmarks and Notes count the rows recorded against anything removed.
	Benchmarks int
	Notes      int
	// LastConfigOfModel: removing this Config leaves its model with no config and no offering.
	LastConfigOfModel bool
	Blockers          []RemovalBlocker
	Files             []RemovalFile
	Token             string
}

// RemovalResult reports what happened.
type RemovalResult struct {
	Plan         RemovalPlan
	FilesDeleted []string
	BytesFreed   int64
	// FileErrors maps a path that could not be deleted to the reason. Its catalog row is already gone.
	FileErrors map[string]string
}

// RemovalReferences is everything outside the model/config tree that can name a Config or a file.
type RemovalReferences struct {
	Settings     map[string]string // key -> raw JSON value
	PoolEngines  []PoolEngineRef
	Jobs         []NamedConfigRef
	Reservations []ReservationRef
	Queued       []NamedConfigRef
	Aliases      []ModelAlias
	Benchmarks   []SubjectRef
	Notes        []SubjectRef
}

// PoolEngineRef is a specialist occupant row.
type PoolEngineRef struct {
	Name   string
	Kind   string
	Params map[string]string
}

// NamedConfigRef is a named row that points at a Config by name.
type NamedConfigRef struct {
	Name   string
	Config string
}

// ReservationRef is a reservation row.
type ReservationRef struct {
	Label  string
	Config string
	End    time.Time
}

// SubjectRef identifies the subject of a benchmark or note.
type SubjectRef struct {
	Type string
	ID   int64
}

// RemovalRows are the rows one removal deletes.
type RemovalRows struct {
	ModelID     int64 // 0: the model stays
	ConfigIDs   []int64
	ArtifactIDs []int64 // artifact rows to delete on their own (a Config removal that deletes files)
	Subjects    []SubjectRef
}

// CatalogRemovals applies the rules. It holds no state; build one per use.
type CatalogRemovals struct {
	Cat Catalog
	// Changed, if set, runs after a successful removal.
	Changed func()
	// Loaded reports whether a Config is running, loading or unloading; nil means nothing is.
	Loaded func(name string) bool
	// ModelsDir is the directory artifact files live under; "" keeps every file.
	ModelsDir func() string
	// Now is a test knob.
	Now func() time.Time
}

type removalCatalog struct {
	models    []Model
	variants  []Variant
	configs   []Config
	artifacts []Artifact
	offerings []Offering
	refs      RemovalReferences
}

func (w CatalogRemovals) read(ctx context.Context) (rc removalCatalog, err error) {
	if rc.models, err = w.Cat.ListModels(ctx); err != nil {
		return rc, err
	}
	if rc.variants, err = w.Cat.ListVariants(ctx); err != nil {
		return rc, err
	}
	if rc.configs, err = w.Cat.ListConfigs(ctx); err != nil {
		return rc, err
	}
	if rc.artifacts, err = w.Cat.ListArtifacts(ctx); err != nil {
		return rc, err
	}
	if rc.offerings, err = w.Cat.ListOfferings(ctx); err != nil {
		return rc, err
	}
	rc.refs, err = w.Cat.RemovalReferences(ctx)
	return rc, err
}

// Plan says what removing the model or config with this id would do.
func (w CatalogRemovals) Plan(ctx context.Context, kind string, id int64) (RemovalPlan, error) {
	p, _, err := w.plan(ctx, kind, id)
	return p, err
}

func (w CatalogRemovals) plan(ctx context.Context, kind string, id int64) (RemovalPlan, removalCatalog, error) {
	p := RemovalPlan{Kind: kind, ID: id, Configs: []RemovalConfig{}, Offerings: []string{}, Aliases: []string{},
		Blockers: []RemovalBlocker{}, Files: []RemovalFile{}}
	rc, err := w.read(ctx)
	if err != nil {
		return p, rc, err
	}
	variantModel := map[int64]int64{}
	for _, v := range rc.variants {
		variantModel[v.ID] = v.ModelID
	}
	modelName := map[int64]string{}
	for _, m := range rc.models {
		modelName[m.ID] = m.Name
	}

	goingConfig := map[int64]bool{}
	goingArtifact := map[int64]bool{} // artifact rows whose files are candidates
	goingVariant := map[int64]bool{}
	switch kind {
	case RemoveKindModel:
		name, ok := modelName[id]
		if !ok {
			return p, rc, fmt.Errorf("%w: model %d", ErrNotFound, id)
		}
		p.Name, p.ModelID, p.ModelName = name, id, name
		for _, v := range rc.variants {
			if v.ModelID == id {
				goingVariant[v.ID] = true
			}
		}
		p.Variants = len(goingVariant)
		for _, c := range rc.configs {
			if goingVariant[c.VariantID] {
				goingConfig[c.ID] = true
			}
		}
		for _, a := range rc.artifacts {
			if goingVariant[a.VariantID] {
				goingArtifact[a.ID] = true
			}
		}
		for _, o := range rc.offerings {
			if o.ModelID == id {
				p.Offerings = append(p.Offerings, o.ProviderName)
			}
		}
	case RemoveKindConfig:
		var target *Config
		for i := range rc.configs {
			if rc.configs[i].ID == id {
				target = &rc.configs[i]
			}
		}
		if target == nil {
			return p, rc, fmt.Errorf("%w: config %d", ErrNotFound, id)
		}
		p.Name = target.Name
		p.ModelID = variantModel[target.VariantID]
		p.ModelName = modelName[p.ModelID]
		goingConfig[id] = true
		// Only this config's own files, and only those no other config uses.
		own := []int64{target.WeightArtifactID, target.MMProjArtifactID}
		for _, aid := range own {
			if aid == 0 {
				continue
			}
			shared := false
			for _, c := range rc.configs {
				if c.ID != id && (c.WeightArtifactID == aid || c.MMProjArtifactID == aid) {
					shared = true
				}
			}
			if !shared {
				goingArtifact[aid] = true
			}
		}
		others := 0
		for _, c := range rc.configs {
			if c.ID != id && variantModel[c.VariantID] == p.ModelID {
				others++
			}
		}
		offered := false
		for _, o := range rc.offerings {
			offered = offered || o.ModelID == p.ModelID
		}
		p.LastConfigOfModel = others == 0 && !offered
	default:
		return p, rc, fmt.Errorf("store: unknown removal kind %q", kind)
	}

	names := map[string]bool{}
	for _, c := range rc.configs {
		if goingConfig[c.ID] {
			p.Configs = append(p.Configs, RemovalConfig{ID: c.ID, Name: c.Name})
			names[c.Name] = true
		}
	}
	sort.Slice(p.Configs, func(i, j int) bool { return p.Configs[i].Name < p.Configs[j].Name })
	for _, a := range rc.refs.Aliases {
		if goingConfig[a.ConfigID] {
			p.Aliases = append(p.Aliases, a.Name)
		}
	}
	p.Blockers = w.blockers(rc, p.Configs, names, goingConfig, goingArtifact)
	p.Files = w.files(rc, goingConfig, goingArtifact)
	for _, s := range subjects(rc, kind, id, goingConfig, goingVariant) {
		for _, b := range rc.refs.Benchmarks {
			if b == s {
				p.Benchmarks++
			}
		}
		for _, n := range rc.refs.Notes {
			if n == s {
				p.Notes++
			}
		}
	}
	p.Token = planToken(p)
	return p, rc, nil
}

// subjects lists the benchmark/note subjects that stop existing.
func subjects(rc removalCatalog, kind string, id int64, goingConfig, goingVariant map[int64]bool) []SubjectRef {
	var out []SubjectRef
	for cid := range goingConfig {
		out = append(out, SubjectRef{Type: "config", ID: cid})
	}
	if kind == RemoveKindModel {
		out = append(out, SubjectRef{Type: "model", ID: id})
		for vid := range goingVariant {
			out = append(out, SubjectRef{Type: "variant", ID: vid})
		}
		for _, o := range rc.offerings {
			if o.ModelID == id {
				out = append(out, SubjectRef{Type: "offering", ID: o.ID})
			}
		}
	}
	return out
}

func (w CatalogRemovals) blockers(rc removalCatalog, configs []RemovalConfig, names map[string]bool, goingConfig, goingArtifact map[int64]bool) []RemovalBlocker {
	out := []RemovalBlocker{}
	now := time.Now()
	if w.Now != nil {
		now = w.Now()
	}
	for _, c := range configs {
		if w.Loaded != nil && w.Loaded(c.Name) {
			out = append(out, RemovalBlocker{Code: BlockConfigLoaded, Config: c.Name})
		}
		for _, e := range rc.refs.PoolEngines {
			if e.Params["config"] == c.Name {
				out = append(out, RemovalBlocker{Code: BlockPoolEngine, Config: c.Name, Name: e.Name})
			}
		}
		for _, j := range rc.refs.Jobs {
			if j.Config == c.Name {
				out = append(out, RemovalBlocker{Code: BlockScheduledJob, Config: c.Name, Name: j.Name})
			}
		}
		for _, r := range rc.refs.Reservations {
			if r.Config == c.Name && r.End.After(now) {
				out = append(out, RemovalBlocker{Code: BlockReservation, Config: c.Name, Name: r.Label})
			}
		}
		for _, q := range rc.refs.Queued {
			if q.Config == c.Name {
				out = append(out, RemovalBlocker{Code: BlockQueuedLoad, Config: c.Name, Name: q.Name})
			}
		}
		for _, key := range sortedKeys(rc.refs.Settings) {
			if stateSettings[key] {
				continue
			}
			if jsonHasString(rc.refs.Settings[key], func(s string) bool { return s == c.Name }) {
				out = append(out, RemovalBlocker{Code: BlockSetting, Config: c.Name, Name: key})
			}
		}
	}
	// A config that stays but uses a file row that would go.
	for _, c := range rc.configs {
		if goingConfig[c.ID] {
			continue
		}
		if goingArtifact[c.WeightArtifactID] || goingArtifact[c.MMProjArtifactID] {
			out = append(out, RemovalBlocker{Code: BlockSharedArtifact, Name: c.Name})
		}
	}
	return out
}

// stateSettings are settings keys that record what happened; a Config name inside one is history, not a dependency.
var stateSettings = map[string]bool{"profile.night": true, "maintenance.state": true}

var shardRE = regexp.MustCompile(`^(.*)-\d{5}-of-(\d{5})\.gguf$`)

// shardPaths returns every shard of a split GGUF, or just path.
func shardPaths(path string) []string {
	m := shardRE.FindStringSubmatch(path)
	if m == nil {
		return []string{path}
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n < 1 {
		return []string{path}
	}
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, fmt.Sprintf("%s-%05d-of-%s.gguf", m[1], i, m[2]))
	}
	return out
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (w CatalogRemovals) files(rc removalCatalog, goingConfig, goingArtifact map[int64]bool) []RemovalFile {
	root := ""
	if w.ModelsDir != nil {
		root = w.ModelsDir()
	}
	abs := func(p string) string {
		if p == "" {
			return ""
		}
		if !filepath.IsAbs(p) {
			if root == "" {
				return ""
			}
			p = filepath.Join(root, p)
		}
		return filepath.Clean(p)
	}
	byPath := map[string]*RemovalFile{}
	var order []string
	for _, a := range rc.artifacts {
		if !goingArtifact[a.ID] {
			continue
		}
		full := abs(a.FilePath)
		if full == "" {
			continue
		}
		for _, sp := range shardPaths(full) {
			f := byPath[sp]
			if f == nil {
				f = &RemovalFile{Path: sp}
				byPath[sp] = f
				order = append(order, sp)
			}
			f.Artifacts = append(f.Artifacts, a.ID)
		}
	}
	sort.Strings(order)

	// Everything that keeps referring to a path after the removal.
	type ref struct{ path, by, why string }
	var refs []ref
	for _, a := range rc.artifacts {
		if goingArtifact[a.ID] {
			continue
		}
		if full := abs(a.FilePath); full != "" {
			for _, sp := range shardPaths(full) {
				refs = append(refs, ref{sp, "artifact " + strconv.FormatInt(a.ID, 10), KeepShared})
			}
		}
	}
	for _, c := range rc.configs {
		if goingConfig[c.ID] {
			continue
		}
		for _, arg := range c.ExtraArgs {
			if i := strings.Index(arg, "="); strings.HasPrefix(arg, "-") && i >= 0 {
				arg = arg[i+1:]
			}
			if strings.ContainsRune(arg, filepath.Separator) || strings.Contains(arg, ".") {
				refs = append(refs, ref{abs(arg), c.Name, KeepReferenced})
			}
		}
	}
	for _, e := range rc.refs.PoolEngines {
		for _, v := range e.Params {
			if strings.ContainsRune(v, filepath.Separator) || strings.Contains(v, ".") {
				refs = append(refs, ref{abs(v), e.Name, KeepReferenced})
			}
		}
	}
	for _, key := range sortedKeys(rc.refs.Settings) {
		jsonHasString(rc.refs.Settings[key], func(s string) bool {
			if strings.ContainsRune(s, filepath.Separator) || strings.Contains(s, ".") {
				refs = append(refs, ref{abs(s), key, KeepReferenced})
			}
			return false
		})
	}

	cleanRoot := filepath.Clean(root)
	realRoot, rootErr := filepath.EvalSymlinks(cleanRoot)
	out := make([]RemovalFile, 0, len(order))
	for _, path := range order {
		f := *byPath[path]
		info, err := os.Lstat(path)
		switch {
		case root == "" || rootErr != nil || path == cleanRoot || !within(path, cleanRoot):
			f.Keep = KeepOutside
		case errors.Is(err, fs.ErrNotExist):
			f.Keep = KeepMissing
		case err != nil:
			f.Keep, f.KeptBy = KeepOutside, err.Error()
		case info.Mode()&fs.ModeSymlink != 0:
			f.Keep = KeepSymlink
		default:
			f.IsDir = info.IsDir()
			f.Bytes = pathBytes(path, info)
			realParent, perr := filepath.EvalSymlinks(filepath.Dir(path))
			if perr != nil || !within(realParent, realRoot) {
				f.Keep = KeepOutside
				break
			}
			f.Delete = true
			for _, r := range refs {
				// The same file, a file inside the directory being deleted, or a directory that contains it. Only paths below the models root
				// count: the root itself, and anything above it (a build root such as /opt/forge), would otherwise "refer to" every file.
				if r.path != "" && r.path != cleanRoot && within(r.path, cleanRoot) && (within(r.path, path) || within(path, r.path)) {
					f.Delete, f.Keep, f.KeptBy = false, r.why, r.by
					break
				}
			}
		}
		out = append(out, f)
	}
	return out
}

// pathBytes is the size of a file, or of everything in a directory (unreadable entries count as nothing: the size is for display).
func pathBytes(path string, info fs.FileInfo) int64 {
	if !info.IsDir() {
		return info.Size()
	}
	var total int64
	walkErr := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, ierr := d.Info(); ierr == nil {
			total += fi.Size()
		}
		return nil
	})
	if walkErr != nil {
		return 0
	}
	return total
}

// planToken covers what the operator confirms: which configs go and which files would be deleted.
func planToken(p RemovalPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%d", p.Kind, p.ID)
	for _, c := range p.Configs {
		fmt.Fprintf(&b, "|c%d", c.ID)
	}
	for _, f := range p.Files {
		if f.Delete {
			fmt.Fprintf(&b, "|f%s:%d", f.Path, f.Bytes)
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// Execute removes the model or config. token is the Plan the caller confirmed; it is required to delete files.
func (w CatalogRemovals) Execute(ctx context.Context, kind string, id int64, deleteFiles bool, token string) (RemovalResult, error) {
	p, rc, err := w.plan(ctx, kind, id)
	res := RemovalResult{Plan: p, FilesDeleted: []string{}, FileErrors: map[string]string{}}
	if err != nil {
		return res, err
	}
	if len(p.Blockers) > 0 {
		return res, ErrRemovalBlocked
	}
	if (deleteFiles && token == "") || (token != "" && token != p.Token) {
		return res, ErrRemovalChanged
	}
	rows := RemovalRows{}
	goingConfig, goingVariant := map[int64]bool{}, map[int64]bool{}
	for _, c := range p.Configs {
		rows.ConfigIDs = append(rows.ConfigIDs, c.ID)
		goingConfig[c.ID] = true
	}
	if kind == RemoveKindModel {
		rows.ModelID = id
		for _, v := range rc.variants {
			if v.ModelID == id {
				goingVariant[v.ID] = true
			}
		}
	} else if deleteFiles {
		// An artifact row goes with its files: every one of its paths is deleted (or already gone).
		kept := map[int64]bool{}
		seen := map[int64]bool{}
		for _, f := range p.Files {
			for _, a := range f.Artifacts {
				seen[a] = true
				if !f.Delete && f.Keep != KeepMissing {
					kept[a] = true
				}
			}
		}
		for a := range seen {
			if !kept[a] {
				rows.ArtifactIDs = append(rows.ArtifactIDs, a)
			}
		}
	}
	rows.Subjects = subjects(rc, kind, id, goingConfig, goingVariant)
	if err := w.Cat.RemoveCatalogRows(ctx, rows); err != nil {
		return res, err
	}
	if w.Changed != nil {
		w.Changed()
	}
	if !deleteFiles {
		return res, nil
	}
	root := filepath.Clean(w.ModelsDir())
	for _, f := range p.Files {
		if !f.Delete {
			continue
		}
		var err error
		if f.IsDir {
			err = os.RemoveAll(f.Path)
		} else {
			err = os.Remove(f.Path)
		}
		if err != nil {
			res.FileErrors[f.Path] = err.Error()
			continue
		}
		res.FilesDeleted = append(res.FilesDeleted, f.Path)
		res.BytesFreed += f.Bytes
		// Tidy directories the deletion emptied. os.Remove refuses a directory that still has anything in it, which ends the climb.
		for dir := filepath.Dir(f.Path); dir != root && within(dir, root); dir = filepath.Dir(dir) {
			if os.Remove(dir) != nil {
				break
			}
		}
	}
	return res, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// jsonHasString reports whether any string value inside raw satisfies match. A value that is not JSON is matched as a plain string.
func jsonHasString(raw string, match func(string) bool) bool {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return match(raw)
	}
	var walk func(any) bool
	walk = func(v any) bool {
		switch x := v.(type) {
		case string:
			return match(x)
		case []any:
			for _, e := range x {
				if walk(e) {
					return true
				}
			}
		case map[string]any:
			for _, e := range x {
				if walk(e) {
					return true
				}
			}
		}
		return false
	}
	return walk(v)
}
