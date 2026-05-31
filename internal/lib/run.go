package lib

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

func Run(cfg Config) error {
	if cfg.ModuleBase == "" {
		base, err := detectModuleBase(cfg.BaseOutputDir)
		if err != nil {
			return fmt.Errorf("module base not set: pass --module-base, or run inside a git repo with an \"origin\" remote (%w)", err)
		}
		cfg.ModuleBase = base
	}

	pkgPath := filepath.Join(cfg.BaseOutputDir, "package.json")
	f, err := os.Open(pkgPath)
	if err != nil {
		return err
	}
	defer f.Close()

	var pkg struct {
		Dependencies Dependencies `json:"dependencies"`
	}
	if err := json.NewDecoder(f).Decode(&pkg); err != nil {
		return err
	}

	for _, dep := range pkg.Dependencies {
		if err := processDependency(cfg, dep); err != nil {
			return fmt.Errorf("processing %s: %w", dep.Name, err)
		}
	}
	return nil
}

// detectModuleBase reads the "origin" git remote of dir and converts the URL
// to a Go-style module path host/owner/repo. Handles the two common forms:
//
//	git@github.com:owner/repo.git
//	https://github.com/owner/repo.git
func detectModuleBase(dir string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	url := strings.TrimSpace(string(out))
	if url == "" {
		return "", fmt.Errorf("empty origin remote URL")
	}

	switch {
	case strings.HasPrefix(url, "git@"):
		// git@host:owner/repo
		url = strings.TrimPrefix(url, "git@")
		url = strings.Replace(url, ":", "/", 1)
	default:
		// strip a leading scheme://[user@]
		for _, scheme := range []string{"https://", "http://", "ssh://", "git://"} {
			url = strings.TrimPrefix(url, scheme)
		}
		if at := strings.Index(url, "@"); at != -1 {
			if slash := strings.Index(url, "/"); slash == -1 || at < slash {
				url = url[at+1:]
			}
		}
	}
	url = strings.TrimSuffix(url, ".git")
	url = strings.TrimSuffix(url, "/")

	if !strings.Contains(url, "/") {
		return "", fmt.Errorf("could not parse remote URL %q into a module path", url)
	}
	return url, nil
}

func processDependency(cfg Config, dep Dependency) error {
	requestedVersion := normalizeSemver(stripRangePrefix(dep.VersionRange))
	relPath := npmNameToModulePath(dep.Name)
	// Go's semantic import versioning: majors >= 2 must have a "/vN" suffix
	// in the module path. We apply it only to the go.mod module line, keeping
	// the source tree flat so its git history tracks across major bumps.
	modulePath := path.Join(cfg.ModuleBase, relPath, majorVersionSuffix(requestedVersion))
	outDir := filepath.Join(cfg.BaseOutputDir, filepath.FromSlash(relPath))

	// Idempotency: if npmpackage.json already records the requested version,
	// nothing on disk needs to change. We don't even hit the npm registry.
	if existing, ok := readNpmPackageVersion(outDir); ok && existing == requestedVersion {
		return nil
	}

	npmv, err := FetchPackageVersion(dep.Name, stripRangePrefix(dep.VersionRange))
	if err != nil {
		return err
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	// Drop any stale tarball contents from a previous version before unpacking
	// the new one (files that existed before but are absent in the new tarball
	// would otherwise linger).
	if err := os.RemoveAll(filepath.Join(outDir, "package")); err != nil {
		return err
	}

	if err := DownloadTarballAndUnpack(npmv.Dist, outDir); err != nil {
		return err
	}

	b, err := json.MarshalIndent(npmv, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := writeTextFile(filepath.Join(outDir, "npmpackage.json"), b); err != nil {
		return err
	}

	// The Hugo mount target keeps the original npm name (including any "@"
	// scope prefix) so user code can write `import x from "@scope/pkg"`. Only
	// Go module paths need the "@" stripped; the virtual assets path doesn't.
	if err := writeHugoConfig(outDir, dep.Name); err != nil {
		return err
	}

	// `hugo mod init` refuses to overwrite an existing go.mod. If one already
	// exists, rewrite just the module statement if needed — that handles a
	// major-version bump that changes the "/vN" suffix without touching any
	// require/replace lines the user may have added.
	goModPath := filepath.Join(outDir, "go.mod")
	if _, err := os.Stat(goModPath); err == nil {
		return ensureGoModModulePath(goModPath, modulePath)
	}
	return runHugoModInit(outDir, modulePath)
}

func ensureGoModModulePath(path, want string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	f, err := modfile.Parse(path, b, nil)
	if err != nil {
		return err
	}
	if f.Module != nil && f.Module.Mod.Path == want {
		return nil
	}
	if err := f.AddModuleStmt(want); err != nil {
		return err
	}
	out, err := f.Format()
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

func readNpmPackageVersion(outDir string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(outDir, "npmpackage.json"))
	if err != nil {
		return "", false
	}
	var p struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return "", false
	}
	return p.Version, p.Version != ""
}

// majorVersionSuffix returns "vN" when version's major is >= 2, matching
// Go's semantic import versioning. v0 and v1 share the unsuffixed module
// path, so it returns "" for those (and for an unparseable version).
func majorVersionSuffix(version string) string {
	major := semver.Major(version)
	if major == "" {
		return ""
	}
	n, err := strconv.Atoi(strings.TrimPrefix(major, "v"))
	if err != nil || n < 2 {
		return ""
	}
	return major
}

// npmNameToModulePath converts an npm package name to a Go/Hugo module path.
// Go modules disallow "@", so a scoped name like "@alpinejs/focus" becomes
// "alpinejs/focus". An unscoped "alpinejs" and a scoped "@alpinejs/X" end up
// as sibling/nested Go modules, which Go and Hugo handle via longest-prefix
// resolution.
func npmNameToModulePath(name string) string {
	return strings.TrimPrefix(name, "@")
}

func runHugoModInit(dir, modulePath string) error {
	cmd := exec.Command("hugo", "mod", "init", modulePath)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("hugo mod init %s: %w: %s", modulePath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// esmDirCandidates are directories (relative to the unpacked tarball's
// "package/" root) that commonly contain a prebuilt ESM build. Mounted whole
// so ESBuild can resolve internal imports.
var esmDirCandidates = []string{
	"dist/esm",
	"dist/es",
	"dist/module",
	"esm",
	"es",
}

// naturalEntryNames are filenames Hugo's bare-specifier resolver can pick up
// on its own from a mounted directory. If the mounted ESM dir already
// contains one of these, no extra entry mount is needed.
var naturalEntryNames = []string{"index.js", "index.esm.js", "index.ts"}

// canonicalEntryName is the filename we expose the ESM entry as when the
// mounted dir doesn't already have a natural entry. Hugo's resolver knows to
// look for this name when given a bare specifier like "@hotwired/turbo".
const canonicalEntryName = "index.esm.js"

func writeHugoConfig(outDir, npmName string) error {
	pkgRoot := filepath.Join(outDir, "package")

	esmDir, err := resolveESMDir(pkgRoot)
	if err != nil {
		return err
	}

	dirSource := path.Join("package", esmDir)
	dirTarget := path.Join("assets", npmName)

	var b strings.Builder
	fmt.Fprintf(&b, "[[module.mounts]]\n  source = %q\n  target = %q\n", dirSource, dirTarget)

	if !hasNaturalEntry(pkgRoot, esmDir) {
		if entryFile, ok := moduleEntryWithin(pkgRoot, esmDir); ok {
			entrySource := path.Join("package", esmDir, entryFile)
			entryTarget := path.Join("assets", npmName, canonicalEntryName)
			fmt.Fprintf(&b, "\n[[module.mounts]]\n  source = %q\n  target = %q\n", entrySource, entryTarget)
		}
	}

	return writeTextFile(filepath.Join(outDir, "hugo.toml"), []byte(b.String()))
}

func hasNaturalEntry(pkgRoot, esmDir string) bool {
	for _, name := range naturalEntryNames {
		if _, err := os.Stat(filepath.Join(pkgRoot, filepath.FromSlash(esmDir), name)); err == nil {
			return true
		}
	}
	return false
}

// moduleEntryWithin returns the package.json "module" entry path expressed
// relative to esmDir, or false if the field is missing or points outside
// esmDir (in which case we can't safely use it as an alias inside the dir
// mount without breaking its sibling imports).
func moduleEntryWithin(pkgRoot, esmDir string) (string, bool) {
	f, err := os.Open(filepath.Join(pkgRoot, "package.json"))
	if err != nil {
		return "", false
	}
	defer f.Close()

	var p struct {
		Module string `json:"module"`
	}
	if err := json.NewDecoder(f).Decode(&p); err != nil {
		return "", false
	}
	if p.Module == "" {
		return "", false
	}
	rel := strings.TrimPrefix(p.Module, "./")
	prefix := esmDir + "/"
	if !strings.HasPrefix(rel, prefix) {
		return "", false
	}
	return strings.TrimPrefix(rel, prefix), true
}

// resolveESMDir picks a directory inside the unpacked tarball whose contents
// should be exposed to Hugo as the package's ESM entry. It prefers a
// conventional prebuilt subdirectory (dist/esm, dist/es, ...); if none exists
// it falls back to the directory containing the "module" field from
// package.json (e.g. dist/module.esm.js -> dist).
func resolveESMDir(pkgRoot string) (string, error) {
	for _, c := range esmDirCandidates {
		if st, err := os.Stat(filepath.Join(pkgRoot, filepath.FromSlash(c))); err == nil && st.IsDir() {
			return c, nil
		}
	}

	f, err := os.Open(filepath.Join(pkgRoot, "package.json"))
	if err != nil {
		return "", err
	}
	defer f.Close()

	var p struct {
		Module string `json:"module"`
	}
	if err := json.NewDecoder(f).Decode(&p); err != nil {
		return "", err
	}
	if p.Module == "" {
		return "", fmt.Errorf("no prebuilt ESM directory found in %s (looked for %v) and package.json has no \"module\" field", pkgRoot, esmDirCandidates)
	}

	dir := path.Dir(strings.TrimPrefix(p.Module, "./"))
	if dir == "" || dir == "." {
		return "", fmt.Errorf("package.json \"module\" field %q resolves to the package root; refusing to mount the whole package", p.Module)
	}
	return dir, nil
}

// writeTextFile writes data with platform-native line endings so generated
// files round-trip cleanly through git's autocrlf on Windows checkouts.
func writeTextFile(path string, data []byte) error {
	if runtime.GOOS == "windows" {
		data = bytes.ReplaceAll(data, []byte("\n"), []byte("\r\n"))
	}
	return os.WriteFile(path, data, 0o644)
}

func stripRangePrefix(s string) string {
	return strings.TrimLeft(s, "^~><=v ")
}
