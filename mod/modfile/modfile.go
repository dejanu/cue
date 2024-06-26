// Copyright 2023 CUE Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package modfile provides functionality for reading and parsing
// the CUE module file, cue.mod/module.cue.
//
// WARNING: THIS PACKAGE IS EXPERIMENTAL.
// ITS API MAY CHANGE AT ANY TIME.
package modfile

import (
	_ "embed"
	"fmt"
	"slices"
	"strings"
	"sync"

	"cuelang.org/go/internal/mod/semver"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/ast"
	"cuelang.org/go/cue/cuecontext"
	"cuelang.org/go/cue/errors"
	"cuelang.org/go/cue/format"
	"cuelang.org/go/cue/parser"
	"cuelang.org/go/cue/token"
	"cuelang.org/go/internal/cueversion"
	"cuelang.org/go/mod/module"
)

//go:embed schema.cue
var moduleSchemaData string

const schemaFile = "cuelang.org/go/mod/modfile/schema.cue"

// File represents the contents of a cue.mod/module.cue file.
type File struct {
	Module   string          `json:"module"`
	Language *Language       `json:"language,omitempty"`
	Source   *Source         `json:"source,omitempty"`
	Deps     map[string]*Dep `json:"deps,omitempty"`
	versions []module.Version
	// defaultMajorVersions maps from module base path to the
	// major version default for that path.
	defaultMajorVersions map[string]string
	// actualSchemaVersion holds the actual schema version
	// that was used to validate the file. This will be one of the
	// entries in the versions field in schema.cue and
	// is set by the Parse functions.
	actualSchemaVersion string
}

// baseFileVersion is used to decode the language version
// to decide how to decode the rest of the file.
type baseFileVersion struct {
	Language struct {
		Version string `json:"version"`
	} `json:"language"`
}

// Source represents how to transform from a module's
// source to its actual contents.
type Source struct {
	Kind string `json:"kind"`
}

// Validate checks that src is well formed.
func (src *Source) Validate() error {
	switch src.Kind {
	case "git", "self":
		return nil
	}
	return fmt.Errorf("unrecognized source kind %q", src.Kind)
}

// Format returns a formatted representation of f
// in CUE syntax.
func (f *File) Format() ([]byte, error) {
	if len(f.Deps) == 0 && f.Deps != nil {
		// There's no way to get the CUE encoder to omit an empty
		// but non-nil slice (despite the current doc comment on
		// [cue.Context.Encode], so make a copy of f to allow us
		// to do that.
		f1 := *f
		f1.Deps = nil
		f = &f1
	}
	// TODO this could be better:
	// - it should omit the outer braces
	v := cuecontext.New().Encode(f)
	if err := v.Validate(cue.Concrete(true)); err != nil {
		return nil, err
	}
	n := v.Syntax(cue.Concrete(true)).(*ast.StructLit)

	data, err := format.Node(&ast.File{
		Decls: n.Elts,
	})
	if err != nil {
		return nil, fmt.Errorf("cannot format: %v", err)
	}
	// Sanity check that it can be parsed.
	// TODO this could be more efficient by checking all the file fields
	// before formatting the output.
	f1, err := ParseNonStrict(data, "-")
	if err != nil {
		return nil, fmt.Errorf("cannot round-trip module file: %v", strings.TrimSuffix(errors.Details(err, nil), "\n"))
	}
	if f.Language != nil && f1.actualSchemaVersion == "v0.0.0" {
		// It's not a legacy module file (because the language field is present)
		// but we've used the legacy schema to parse it, which means that
		// it's almost certainly a bogus version because all versions
		// we care about fail when there are unknown fields, but the
		// original schema allowed all fields.
		return nil, fmt.Errorf("language version %v is too early for module.cue (need at least %v)", f.Language.Version, EarliestClosedSchemaVersion())
	}
	return data, err
}

type Language struct {
	Version string `json:"version,omitempty"`
}

type Dep struct {
	Version string `json:"v"`
	Default bool   `json:"default,omitempty"`
}

type noDepsFile struct {
	Module string `json:"module"`
}

var (
	moduleSchemaOnce sync.Once // guards the creation of _moduleSchema
	// TODO remove this mutex when https://cuelang.org/issue/2733 is fixed.
	moduleSchemaMutex sync.Mutex // guards any use of _moduleSchema
	_schemas          schemaInfo
	_cueContext       *cue.Context
)

type schemaInfo struct {
	Versions                    map[string]cue.Value `json:"versions"`
	EarliestClosedSchemaVersion string               `json:"earliestClosedSchemaVersion"`
}

// moduleSchemaDo runs f with information about all the schema versions
// present in schema.cue. It does this within a mutex because it is
// not currently allowed to use cue.Value concurrently.
// TODO remove the mutex when https://cuelang.org/issue/2733 is fixed.
func moduleSchemaDo[T any](f func(*cue.Context, *schemaInfo) (T, error)) (T, error) {
	moduleSchemaOnce.Do(func() {
		_cueContext = cuecontext.New()
		schemav := _cueContext.CompileString(moduleSchemaData, cue.Filename(schemaFile))
		if err := schemav.Decode(&_schemas); err != nil {
			panic(fmt.Errorf("internal error: invalid CUE module.cue schema: %v", errors.Details(err, nil)))
		}
	})
	moduleSchemaMutex.Lock()
	defer moduleSchemaMutex.Unlock()
	return f(_cueContext, &_schemas)
}

func lookup(v cue.Value, sels ...cue.Selector) cue.Value {
	return v.LookupPath(cue.MakePath(sels...))
}

// EarliestClosedSchemaVersion returns the earliest module.cue schema version
// that excludes unknown fields. Any version declared in a module.cue file
// should be at least this, because that's when we added the language.version
// field itself.
func EarliestClosedSchemaVersion() string {
	return schemaVersionLimits()[0]
}

// LatestKnownSchemaVersion returns the language version
// associated with the most recent known schema.
func LatestKnownSchemaVersion() string {
	return schemaVersionLimits()[1]
}

var schemaVersionLimits = sync.OnceValue(func() [2]string {
	limits, _ := moduleSchemaDo(func(_ *cue.Context, info *schemaInfo) ([2]string, error) {
		earliest := ""
		latest := ""
		for v := range info.Versions {
			if earliest == "" || semver.Compare(v, earliest) < 0 {
				earliest = v
			}
			if latest == "" || semver.Compare(v, latest) > 0 {
				latest = v
			}
		}
		return [2]string{earliest, latest}, nil
	})
	return limits
})

// Parse verifies that the module file has correct syntax.
// The file name is used for error messages.
// All dependencies must be specified correctly: with major
// versions in the module paths and canonical dependency
// versions.
func Parse(modfile []byte, filename string) (*File, error) {
	return parse(modfile, filename, true)
}

// ParseLegacy parses the legacy version of the module file
// that only supports the single field "module" and ignores all other
// fields.
func ParseLegacy(modfile []byte, filename string) (*File, error) {
	return moduleSchemaDo(func(ctx *cue.Context, _ *schemaInfo) (*File, error) {
		v := ctx.CompileBytes(modfile, cue.Filename(filename))
		if err := v.Err(); err != nil {
			return nil, errors.Wrapf(err, token.NoPos, "invalid module.cue file")
		}
		var f noDepsFile
		if err := v.Decode(&f); err != nil {
			return nil, newCUEError(err, filename)
		}
		return &File{
			Module:              f.Module,
			actualSchemaVersion: "v0.0.0",
		}, nil
	})
}

// ParseNonStrict is like Parse but allows some laxity in the parsing:
//   - if a module path lacks a version, it's taken from the version.
//   - if a non-canonical version is used, it will be canonicalized.
//
// The file name is used for error messages.
func ParseNonStrict(modfile []byte, filename string) (*File, error) {
	return parse(modfile, filename, false)
}

func parse(modfile []byte, filename string, strict bool) (*File, error) {
	file, err := parser.ParseFile(filename, modfile)
	if err != nil {
		return nil, errors.Wrapf(err, token.NoPos, "invalid module.cue file syntax")
	}
	// TODO disallow non-data-mode CUE.

	mf, err := moduleSchemaDo(func(ctx *cue.Context, schemas *schemaInfo) (*File, error) {
		v := ctx.BuildFile(file)
		if err := v.Validate(cue.Concrete(true)); err != nil {
			return nil, errors.Wrapf(err, token.NoPos, "invalid module.cue file value")
		}
		// First determine the declared version of the module file.
		var base baseFileVersion
		if err := v.Decode(&base); err != nil {
			return nil, errors.Wrapf(err, token.NoPos, "cannot determine language version")
		}
		if base.Language.Version == "" {
			// TODO is something different we could do here?
			return nil, fmt.Errorf("no language version declared in module.cue")
		}
		if !semver.IsValid(base.Language.Version) {
			return nil, fmt.Errorf("language version %q in module.cue is not valid semantic version", base.Language.Version)
		}
		if mv, lv := base.Language.Version, cueversion.LanguageVersion(); semver.Compare(mv, lv) > 0 {
			return nil, fmt.Errorf("language version %q declared in module.cue is too new for current language version %q", mv, lv)
		}
		// Now that we're happy we're within bounds, find the latest
		// schema that applies to the declared version.
		latest := ""
		var latestSchema cue.Value
		for vers, schema := range schemas.Versions {
			if semver.Compare(vers, base.Language.Version) > 0 {
				continue
			}
			if latest == "" || semver.Compare(vers, latest) > 0 {
				latest = vers
				latestSchema = schema
			}
		}
		if latest == "" {
			// Should never happen, because there should always
			// be some applicable schema.
			return nil, fmt.Errorf("cannot find schema suitable for reading module file with language version %q", base.Language.Version)
		}
		schema := latestSchema
		v = v.Unify(lookup(schema, cue.Def("#File")))
		if err := v.Validate(); err != nil {
			return nil, newCUEError(err, filename)
		}
		if latest == "v0.0.0" {
			// The chosen schema is the earliest schema which allowed
			// all fields. We don't actually want a module.cue file with
			// an old version to treat those fields as special, so don't try
			// to decode into *File because that will do so.
			// This mirrors the behavior of [ParseLegacy].
			var f noDepsFile
			if err := v.Decode(&f); err != nil {
				return nil, newCUEError(err, filename)
			}
			return &File{
				Module:              f.Module,
				actualSchemaVersion: "v0.0.0",
			}, nil
		}
		var mf File
		if err := v.Decode(&mf); err != nil {
			return nil, errors.Wrapf(err, token.NoPos, "internal error: cannot decode into modFile struct")
		}
		mf.actualSchemaVersion = latest
		return &mf, nil
	})
	if err != nil {
		return nil, err
	}
	mainPath, mainMajor, ok := module.SplitPathVersion(mf.Module)
	if strict && !ok {
		return nil, fmt.Errorf("module path %q in %s does not contain major version", mf.Module, filename)
	}
	if ok {
		if semver.Major(mainMajor) != mainMajor {
			return nil, fmt.Errorf("module path %s in %q should contain the major version only", mf.Module, filename)
		}
	} else if mainPath = mf.Module; mainPath != "" {
		if err := module.CheckPathWithoutVersion(mainPath); err != nil {
			return nil, fmt.Errorf("module path %q in %q is not valid: %v", mainPath, filename, err)
		}
		// There's no main module major version: default to v0.
		mainMajor = "v0"
		// TODO perhaps we'd be better preserving the original?
		mf.Module += "@v0"
	}
	if mf.Language != nil {
		vers := mf.Language.Version
		if !semver.IsValid(vers) {
			return nil, fmt.Errorf("language version %q in %s is not well formed", vers, filename)
		}
		if semver.Canonical(vers) != vers {
			return nil, fmt.Errorf("language version %v in %s is not canonical", vers, filename)
		}
	}
	var versions []module.Version
	// The main module is always the default for its own major version.
	defaultMajorVersions := map[string]string{
		mainPath: mainMajor,
	}
	// Check that major versions match dependency versions.
	for m, dep := range mf.Deps {
		vers, err := module.NewVersion(m, dep.Version)
		if err != nil {
			return nil, fmt.Errorf("invalid module.cue file %s: cannot make version from module %q, version %q: %v", filename, m, dep.Version, err)
		}
		versions = append(versions, vers)
		if strict && vers.Path() != m {
			return nil, fmt.Errorf("invalid module.cue file %s: no major version in %q", filename, m)
		}
		if dep.Default {
			mp := vers.BasePath()
			if _, ok := defaultMajorVersions[mp]; ok {
				return nil, fmt.Errorf("multiple default major versions found for %v", mp)
			}
			defaultMajorVersions[mp] = semver.Major(vers.Version())
		}
	}

	if len(defaultMajorVersions) > 0 {
		mf.defaultMajorVersions = defaultMajorVersions
	}
	mf.versions = versions[:len(versions):len(versions)]
	module.Sort(mf.versions)
	return mf, nil
}

func newCUEError(err error, filename string) error {
	ps := errors.Positions(err)
	for _, p := range ps {
		if errStr := findErrorComment(p); errStr != "" {
			return fmt.Errorf("invalid module.cue file: %s", errStr)
		}
	}
	// TODO we have more potential to improve error messages here.
	return err
}

// findErrorComment finds an error comment in the form
//
//	//error: ...
//
// before the given position.
// This works as a kind of poor-man's error primitive
// so we can customize the error strings when verification
// fails.
func findErrorComment(p token.Pos) string {
	if p.Filename() != schemaFile {
		return ""
	}
	off := p.Offset()
	source := moduleSchemaData
	if off > len(source) {
		return ""
	}
	source, _, ok := cutLast(source[:off], "\n")
	if !ok {
		return ""
	}
	_, errorLine, ok := cutLast(source, "\n")
	if !ok {
		return ""
	}
	errStr, ok := strings.CutPrefix(errorLine, "//error: ")
	if !ok {
		return ""
	}
	return errStr
}

func cutLast(s, sep string) (before, after string, found bool) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return "", s, false
}

// DepVersions returns the versions of all the modules depended on by the
// file. The caller should not modify the returned slice.
//
// This always returns the same value, even if the contents
// of f are changed. If f was not created with [Parse], it returns nil.
func (f *File) DepVersions() []module.Version {
	return slices.Clip(f.versions)
}

// DefaultMajorVersions returns a map from module base path
// to the major version that's specified as the default for that module.
// The caller should not modify the returned map.
func (f *File) DefaultMajorVersions() map[string]string {
	return f.defaultMajorVersions
}
