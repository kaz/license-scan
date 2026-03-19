package main

import (
	"context"
	"crypto/x509"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	pb "deps.dev/api/v3"
	"github.com/alecthomas/kong"
	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	billyutil "github.com/go-git/go-billy/v5/util"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/mattn/go-isatty"
	"github.com/olekukonko/tablewriter"
	"golang.org/x/crypto/ssh"
	"golang.org/x/mod/modfile"
	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

type CLI struct {
	InsecureIgnoreHostKey bool     `help:"Disable SSH host key verification and do not read known_hosts."`
	FallbackToDefault     bool     `help:"When enabled, retry deps.dev lookup with the package default version if the requested version is not found."`
	Format                string   `help:"Output format." enum:"table,csv" default:"table"`
	Targets               []string `arg:"" name:"target" help:"Scan target directories or git repository URLs."`
}

type dependency struct {
	Scope          string
	Name           string
	Version        string
	System         pb.System
	License        string
	QueriedVersion string
	FallbackUsed   bool
}

type manifestResult struct {
	Path         string
	Dependencies []dependency
}

type dependencyVersionKey struct {
	System  pb.System
	Name    string
	Version string
}

type outputRow struct {
	SourceName   string
	ManifestPath string
	Dependency   string
	LibraryName  string
	Version      string
	License      string
}

type licenseLookupResult struct {
	License        string
	QueriedVersion string
	FallbackUsed   bool
}

type scanBundle struct {
	SourceName string
	Results    []manifestResult
}

type cloneFailureError struct {
	repo string
	err  error
}

func (e *cloneFailureError) Error() string {
	return fmt.Sprintf("repository clone failed for %s: %v", e.repo, e.err)
}

func (e *cloneFailureError) Unwrap() error {
	return e.err
}

type statusLine struct {
	mu      sync.Mutex
	writer  io.Writer
	enabled bool
	current string
}

var progress = newStatusLine(os.Stderr)

func newStatusLine(file *os.File) *statusLine {
	return &statusLine{
		writer:  file,
		enabled: isatty.IsTerminal(file.Fd()) || isatty.IsCygwinTerminal(file.Fd()),
	}
}

func (s *statusLine) Update(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.current = message
	if !s.enabled {
		return
	}

	fmt.Fprintf(s.writer, "\r\033[2K%s", message)
}

func (s *statusLine) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.current = ""
	if !s.enabled {
		return
	}

	fmt.Fprint(s.writer, "\r\033[2K")
}

func (s *statusLine) Warnf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()

	message := fmt.Sprintf(format, args...)
	if !s.enabled {
		fmt.Fprintln(s.writer, message)
		return
	}

	fmt.Fprintf(s.writer, "\r\033[2K%s\n", message)
	if s.current != "" {
		fmt.Fprintf(s.writer, "\r\033[2K%s", s.current)
	}
}

func main() {
	var cli CLI
	kong.Parse(&cli,
		kong.Name("license-scan"),
		kong.Description("Scan a directory or git repository for package.json and go.mod files."),
	)
	defer progress.Clear()
	if len(cli.Targets) == 0 {
		exitIfErr(fmt.Errorf("at least one target is required"))
	}

	var bundles []scanBundle
	for i, target := range cli.Targets {
		sourceName := displaySourceName(target)
		progress.Update(fmt.Sprintf("Scanning target %d/%d: %s", i+1, len(cli.Targets), sourceName))
		results, err := scanTarget(target, cli.InsecureIgnoreHostKey)
		if err != nil {
			var cloneErr *cloneFailureError
			if errors.As(err, &cloneErr) {
				progress.Warnf("warning: %v", err)
				continue
			}
			exitIfErr(err)
		}

		bundles = append(bundles, scanBundle{
			SourceName: sourceName,
			Results:    results,
		})
	}

	progress.Update("Resolving licenses...")
	exitIfErr(enrichLicenses(bundles, cli.FallbackToDefault))

	rows := buildOutputRows(bundles)
	progress.Update("Rendering output...")
	progress.Clear()
	exitIfErr(renderOutput(os.Stdout, cli.Format, rows))
	progress.Clear()
}

func scanTarget(target string, insecureIgnoreHostKey bool) ([]manifestResult, error) {
	if isGitRepository(target) {
		repoFS, err := cloneRepository(target, insecureIgnoreHostKey)
		if err != nil {
			return nil, err
		}

		return scanBillyFilesystem(repoFS, "/")
	}

	root, err := filepath.Abs(target)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}

	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", root)
	}

	return scanLocalFilesystem(root)
}

func cloneRepository(repo string, insecureIgnoreHostKey bool) (billy.Filesystem, error) {
	progress.Update(fmt.Sprintf("Cloning repository: %s", displaySourceName(repo)))
	auth, err := authForRepository(repo, insecureIgnoreHostKey)
	if err != nil {
		return nil, err
	}

	repoFS := memfs.New()
	if _, err := git.Clone(memory.NewStorage(), repoFS, &git.CloneOptions{
		URL:   repo,
		Depth: 1,
		Auth:  auth,
	}); err != nil {
		return nil, &cloneFailureError{repo: displaySourceName(repo), err: err}
	}

	return repoFS, nil
}

func scanLocalFilesystem(root string) ([]manifestResult, error) {
	var results []manifestResult
	found := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		name := d.Name()
		if name != "package.json" && name != "go.mod" {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		if err := appendLocalManifestResult(&results, path, rel, name); err != nil {
			return err
		}

		found++
		if found == 1 || found%10 == 0 {
			progress.Update(fmt.Sprintf("Scanning manifests... %d found", found))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	progress.Update(fmt.Sprintf("Scanning manifests... %d found", found))
	return results, nil
}

func scanBillyFilesystem(fs billy.Filesystem, root string) ([]manifestResult, error) {
	var results []manifestResult
	found := 0

	err := billyutil.Walk(fs, root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		name := info.Name()
		if name != "package.json" && name != "go.mod" {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		if err := appendBillyManifestResult(&results, fs, path, rel, name); err != nil {
			return err
		}

		found++
		if found == 1 || found%10 == 0 {
			progress.Update(fmt.Sprintf("Scanning manifests... %d found", found))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	progress.Update(fmt.Sprintf("Scanning manifests... %d found", found))
	return results, nil
}

func appendLocalManifestResult(results *[]manifestResult, path string, rel string, name string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		progress.Warnf("warning: failed to read %s: %v", rel, err)
		return nil
	}

	result, err := parseManifest(rel, name, data)
	if err != nil {
		progress.Warnf("warning: %v", err)
		return nil
	}

	*results = append(*results, result)
	return nil
}

func appendBillyManifestResult(results *[]manifestResult, filesystem billy.Filesystem, path string, rel string, name string) error {
	file, err := filesystem.Open(path)
	if err != nil {
		progress.Warnf("warning: failed to read %s: %v", rel, err)
		return nil
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		progress.Warnf("warning: failed to read %s: %v", rel, err)
		return nil
	}

	result, err := parseManifest(rel, name, data)
	if err != nil {
		progress.Warnf("warning: %v", err)
		return nil
	}

	*results = append(*results, result)
	return nil
}

func parseManifest(rel string, name string, data []byte) (manifestResult, error) {
	switch name {
	case "package.json":
		return parsePackageJSON(rel, data)
	case "go.mod":
		return parseGoMod(rel, data)
	default:
		return manifestResult{}, fmt.Errorf("unsupported manifest: %s", name)
	}
}

func parsePackageJSON(rel string, data []byte) (manifestResult, error) {
	var manifest struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}

	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifestResult{}, fmt.Errorf("failed to parse %s: %w", rel, err)
	}

	dependencies := make([]dependency, 0, len(manifest.Dependencies)+len(manifest.DevDependencies))
	for name, version := range manifest.Dependencies {
		dependencies = append(dependencies, dependency{
			Scope:   "dependencies",
			Name:    name,
			System:  pb.System_NPM,
			Version: version,
		})
	}

	for name, version := range manifest.DevDependencies {
		dependencies = append(dependencies, dependency{
			Scope:   "devDependencies",
			Name:    name,
			System:  pb.System_NPM,
			Version: version,
		})
	}

	sortDependencies(dependencies)

	return manifestResult{
		Path:         rel,
		Dependencies: dependencies,
	}, nil
}

func parseGoMod(rel string, data []byte) (manifestResult, error) {
	mod, err := modfile.Parse(rel, data, nil)
	if err != nil {
		return manifestResult{}, fmt.Errorf("failed to parse %s: %w", rel, err)
	}

	dependencies := make([]dependency, 0, len(mod.Require))
	for _, req := range mod.Require {
		if req.Indirect {
			continue
		}

		dependencies = append(dependencies, dependency{
			Scope:   "require",
			Name:    req.Mod.Path,
			System:  pb.System_GO,
			Version: req.Mod.Version,
		})
	}

	sortDependencies(dependencies)

	return manifestResult{
		Path:         rel,
		Dependencies: dependencies,
	}, nil
}

func sortDependencies(dependencies []dependency) {
	sort.Slice(dependencies, func(i int, j int) bool {
		if dependencies[i].Scope != dependencies[j].Scope {
			return dependencies[i].Scope < dependencies[j].Scope
		}

		return dependencies[i].Name < dependencies[j].Name
	})
}

func enrichLicenses(bundles []scanBundle, fallbackToDefault bool) error {
	keys := make(map[dependencyVersionKey]struct{})

	for i := range bundles {
		for j := range bundles[i].Results {
			for k := range bundles[i].Results[j].Dependencies {
				dep := &bundles[i].Results[j].Dependencies[k]
				version, ok := depsDevLookupVersion(*dep)
				if !ok {
					if fallbackToDefault {
						keys[dependencyVersionKey{
							System: dep.System,
							Name:   dep.Name,
						}] = struct{}{}
						continue
					}

					dep.License = "unresolved-version"
					continue
				}

				key := dependencyVersionKey{
					System:  dep.System,
					Name:    dep.Name,
					Version: version,
				}
				keys[key] = struct{}{}
			}
		}
	}

	if len(keys) == 0 {
		return nil
	}

	client, conn, err := newDepsDevClient()
	if err != nil {
		return err
	}
	defer conn.Close()

	licenses, err := fetchLicenses(client, keys, fallbackToDefault)
	if err != nil {
		return err
	}

	for i := range bundles {
		for j := range bundles[i].Results {
			for k := range bundles[i].Results[j].Dependencies {
				dep := &bundles[i].Results[j].Dependencies[k]
				if dep.License != "" {
					continue
				}

				key := dependencyVersionKey{
					System:  dep.System,
					Name:    dep.Name,
					Version: resolvedVersionForLicenseLookup(*dep),
				}
				if key.Version == "" && fallbackToDefault {
					key = dependencyVersionKey{
						System: dep.System,
						Name:   dep.Name,
					}
				}

				lookup, ok := licenses[key]
				if !ok || lookup.License == "" {
					dep.License = "unknown"
					continue
				}

				dep.License = lookup.License
				dep.QueriedVersion = lookup.QueriedVersion
				dep.FallbackUsed = lookup.FallbackUsed
			}
		}
	}

	return nil
}

func depsDevLookupVersion(dep dependency) (string, bool) {
	switch dep.System {
	case pb.System_GO:
		return dep.Version, dep.Version != ""
	case pb.System_NPM:
		return normalizeNPMVersion(dep.Version)
	default:
		return "", false
	}
}

func resolvedVersionForLicenseLookup(dep dependency) string {
	version, _ := depsDevLookupVersion(dep)
	return version
}

func normalizeNPMVersion(version string) (string, bool) {
	if version == "" {
		return "", false
	}

	version = strings.TrimSpace(version)
	lower := strings.ToLower(version)
	if strings.HasPrefix(lower, "workspace:") {
		return normalizeNPMVersion(strings.TrimSpace(version[len("workspace:"):]))
	}

	if strings.HasPrefix(lower, "file:") ||
		strings.HasPrefix(lower, "link:") ||
		strings.HasPrefix(lower, "git+") ||
		strings.HasPrefix(lower, "github:") ||
		strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://") ||
		strings.HasPrefix(lower, "npm:") {
		return "", false
	}

	for _, separator := range []string{"||", " ", ","} {
		if idx := strings.Index(version, separator); idx >= 0 {
			version = strings.TrimSpace(version[:idx])
			break
		}
	}

	version = strings.TrimLeft(version, "^~<>=v")
	if version == "" {
		return "", false
	}

	if strings.ContainsAny(version, "*xX") {
		return "", false
	}

	return parseNPMSemver(version)
}

func parseNPMSemver(version string) (string, bool) {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return "", false
	}

	for _, part := range parts[:2] {
		if part == "" || !allDigits(part) {
			return "", false
		}
	}

	if parts[2] == "" {
		return "", false
	}

	patch := parts[2]
	for idx, r := range patch {
		if r == '-' || r == '+' {
			if idx > 0 && allDigits(patch[:idx]) {
				return version, true
			}

			return "", false
		}
	}

	if !allDigits(patch) {
		return "", false
	}

	return version, true
}

func allDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}

	return value != ""
}

func newDepsDevClient() (pb.InsightsClient, *grpc.ClientConn, error) {
	certPool, err := x509.SystemCertPool()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load system cert pool: %w", err)
	}

	creds := credentials.NewClientTLSFromCert(certPool, "")
	conn, err := grpc.NewClient("dns:///api.deps.dev:443", grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect to deps.dev: %w", err)
	}

	return pb.NewInsightsClient(conn), conn, nil
}

func fetchLicenses(client pb.InsightsClient, keys map[dependencyVersionKey]struct{}, fallbackToDefault bool) (map[dependencyVersionKey]licenseLookupResult, error) {
	ctx := context.Background()
	g, _ := errgroup.WithContext(ctx)
	g.SetLimit(20)

	limiter := rate.NewLimiter(50, 1)
	licenses := make(map[dependencyVersionKey]licenseLookupResult, len(keys))
	var mu sync.Mutex
	total := len(keys)
	completed := 0
	progress.Update(fmt.Sprintf("Resolving licenses... 0/%d", total))

	for key := range keys {
		key := key
		g.Go(func() error {
			if err := limiter.Wait(ctx); err != nil {
				mu.Lock()
				licenses[key] = licenseLookupResult{
					License:        "lookup-error",
					QueriedVersion: key.Version,
				}
				completed++
				if completed == 1 || completed == total || completed%10 == 0 {
					progress.Update(fmt.Sprintf("Resolving licenses... %d/%d", completed, total))
				}
				mu.Unlock()
				progress.Warnf("warning: deps.dev rate limiter failed for %s@%s: %v", key.Name, key.Version, err)
				return nil
			}

			if key.Version == "" {
				license, resolved, fallbackErr := lookupDefaultVersionLicense(ctx, client, key)
				switch {
				case fallbackErr != nil:
					mu.Lock()
					licenses[key] = licenseLookupResult{
						License:        "lookup-error",
						QueriedVersion: key.Version,
					}
					completed++
					if completed == 1 || completed == total || completed%10 == 0 {
						progress.Update(fmt.Sprintf("Resolving licenses... %d/%d", completed, total))
					}
					mu.Unlock()
					progress.Warnf("warning: deps.dev fallback lookup failed for %s: %v", key.Name, fallbackErr)
					return nil
				case resolved:
					mu.Lock()
					licenses[key] = license
					completed++
					if completed == 1 || completed == total || completed%10 == 0 {
						progress.Update(fmt.Sprintf("Resolving licenses... %d/%d", completed, total))
					}
					mu.Unlock()
					return nil
				default:
					mu.Lock()
					licenses[key] = licenseLookupResult{
						License:        "not-found",
						QueriedVersion: key.Version,
					}
					completed++
					if completed == 1 || completed == total || completed%10 == 0 {
						progress.Update(fmt.Sprintf("Resolving licenses... %d/%d", completed, total))
					}
					mu.Unlock()
					return nil
				}
			}

			resp, err := client.GetVersion(ctx, &pb.GetVersionRequest{
				VersionKey: &pb.VersionKey{
					System:  key.System,
					Name:    key.Name,
					Version: key.Version,
				},
			})
			if err != nil {
				switch status.Code(err) {
				case codes.NotFound:
					if fallbackToDefault {
						license, resolved, fallbackErr := lookupDefaultVersionLicense(ctx, client, key)
						switch {
						case fallbackErr != nil:
							mu.Lock()
							licenses[key] = licenseLookupResult{
								License:        "lookup-error",
								QueriedVersion: key.Version,
							}
							completed++
							if completed == 1 || completed == total || completed%10 == 0 {
								progress.Update(fmt.Sprintf("Resolving licenses... %d/%d", completed, total))
							}
							mu.Unlock()
							progress.Warnf("warning: deps.dev fallback lookup failed for %s@%s: %v", key.Name, key.Version, fallbackErr)
							return nil
						case resolved:
							mu.Lock()
							licenses[key] = license
							completed++
							if completed == 1 || completed == total || completed%10 == 0 {
								progress.Update(fmt.Sprintf("Resolving licenses... %d/%d", completed, total))
							}
							mu.Unlock()
							return nil
						}
					}

					mu.Lock()
					licenses[key] = licenseLookupResult{
						License:        "not-found",
						QueriedVersion: key.Version,
					}
					completed++
					if completed == 1 || completed == total || completed%10 == 0 {
						progress.Update(fmt.Sprintf("Resolving licenses... %d/%d", completed, total))
					}
					mu.Unlock()
					return nil
				default:
					mu.Lock()
					licenses[key] = licenseLookupResult{
						License:        "lookup-error",
						QueriedVersion: key.Version,
					}
					completed++
					if completed == 1 || completed == total || completed%10 == 0 {
						progress.Update(fmt.Sprintf("Resolving licenses... %d/%d", completed, total))
					}
					mu.Unlock()
					progress.Warnf("warning: deps.dev lookup failed for %s@%s: %v", key.Name, key.Version, err)
					return nil
				}
			}

			mu.Lock()
			licenses[key] = licenseLookupResult{
				License:        formatLicenses(resp),
				QueriedVersion: key.Version,
			}
			completed++
			if completed == 1 || completed == total || completed%10 == 0 {
				progress.Update(fmt.Sprintf("Resolving licenses... %d/%d", completed, total))
			}
			mu.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	return licenses, nil
}

func lookupDefaultVersionLicense(ctx context.Context, client pb.InsightsClient, key dependencyVersionKey) (licenseLookupResult, bool, error) {
	pkg, err := client.GetPackage(ctx, &pb.GetPackageRequest{
		PackageKey: &pb.PackageKey{
			System: key.System,
			Name:   key.Name,
		},
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return licenseLookupResult{}, false, nil
		}
		return licenseLookupResult{}, false, err
	}

	defaultVersion := defaultPackageVersion(pkg)
	if defaultVersion == "" {
		return licenseLookupResult{}, false, nil
	}

	resp, err := client.GetVersion(ctx, &pb.GetVersionRequest{
		VersionKey: &pb.VersionKey{
			System:  key.System,
			Name:    key.Name,
			Version: defaultVersion,
		},
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return licenseLookupResult{}, false, nil
		}
		return licenseLookupResult{}, false, err
	}

	return licenseLookupResult{
		License:        formatLicenses(resp),
		QueriedVersion: defaultVersion,
		FallbackUsed:   true,
	}, true, nil
}

func defaultPackageVersion(pkg *pb.Package) string {
	if pkg == nil {
		return ""
	}

	for _, version := range pkg.GetVersions() {
		if version != nil && version.GetIsDefault() {
			if key := version.GetVersionKey(); key != nil {
				return key.GetVersion()
			}
		}
	}

	return ""
}

func formatLicenses(version *pb.Version) string {
	if version == nil {
		return "unknown"
	}

	if len(version.GetLicenses()) == 0 {
		return "unknown"
	}

	return joinUniqueSorted(version.GetLicenses())
}

func joinUniqueSorted(values []string) string {
	unique := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}

		unique[value] = struct{}{}
	}

	if len(unique) == 0 {
		return "unknown"
	}

	sorted := make([]string, 0, len(unique))
	for value := range unique {
		sorted = append(sorted, value)
	}

	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

func displayQueriedVersion(dep dependency) string {
	if dep.QueriedVersion == "" {
		return ""
	}

	if dep.FallbackUsed {
		return dep.QueriedVersion + " *"
	}

	return dep.QueriedVersion
}

func buildOutputRows(bundles []scanBundle) []outputRow {
	rows := make([]outputRow, 0)
	for _, bundle := range bundles {
		for _, result := range bundle.Results {
			for _, dep := range result.Dependencies {
				rows = append(rows, outputRow{
					SourceName:   bundle.SourceName,
					ManifestPath: result.Path,
					Dependency:   dep.Scope,
					LibraryName:  dep.Name,
					Version:      displayQueriedVersion(dep),
					License:      dep.License,
				})
			}
		}
	}

	return rows
}

func renderOutput(w io.Writer, format string, rows []outputRow) error {
	switch format {
	case "csv":
		return renderCSV(w, rows)
	case "table":
		return renderTable(w, rows)
	default:
		return fmt.Errorf("unsupported format: %s", format)
	}
}

func renderCSV(w io.Writer, rows []outputRow) error {
	writer := csv.NewWriter(w)
	if err := writer.Write([]string{
		"source",
		"manifest",
		"dependency_type",
		"library",
		"version",
		"license",
	}); err != nil {
		return err
	}

	for _, row := range rows {
		if err := writer.Write([]string{
			row.SourceName,
			row.ManifestPath,
			row.Dependency,
			row.LibraryName,
			row.Version,
			row.License,
		}); err != nil {
			return err
		}
	}

	writer.Flush()
	return writer.Error()
}

func renderTable(w io.Writer, rows []outputRow) error {
	table := tablewriter.NewTable(w)
	table.Header([]string{
		"source",
		"manifest",
		"dependency_type",
		"library",
		"version",
		"license",
	})

	for _, row := range rows {
		table.Append([]string{
			row.SourceName,
			row.ManifestPath,
			row.Dependency,
			row.LibraryName,
			row.Version,
			row.License,
		})
	}

	table.Render()
	return nil
}

func displaySourceName(target string) string {
	if isGitRepository(target) {
		return repositoryName(target)
	}

	root, err := filepath.Abs(target)
	if err != nil {
		return filepath.Base(strings.TrimRight(target, "/"))
	}

	return filepath.Base(root)
}

func repositoryName(target string) string {
	if strings.HasPrefix(target, "ssh://") || strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		u, err := url.Parse(target)
		if err == nil {
			path := trimGitSuffix(strings.Trim(strings.TrimSpace(u.Path), "/"))
			if u.Host != "" && path != "" {
				return u.Host + "/" + path
			}
			if path != "" {
				return path
			}
		}
	}

	if isSCPLikeGitURL(target) {
		host := ""
		if at := strings.Index(target, "@"); at >= 0 && at+1 < len(target) {
			rest := target[at+1:]
			if colon := strings.Index(rest, ":"); colon >= 0 {
				host = rest[:colon]
			}
		}
		if idx := strings.Index(target, ":"); idx >= 0 && idx+1 < len(target) {
			path := trimGitSuffix(strings.Trim(strings.TrimSpace(target[idx+1:]), "/"))
			if host != "" && path != "" {
				return host + "/" + path
			}
			if path != "" {
				return path
			}
		}
	}

	return trimGitSuffix(pathBase(target))
}

func pathBase(value string) string {
	value = strings.TrimRight(value, "/")
	if value == "" {
		return value
	}

	return filepath.Base(value)
}

func trimGitSuffix(value string) string {
	return strings.TrimSuffix(value, ".git")
}

func isGitRepository(target string) bool {
	return strings.HasPrefix(target, "http://") ||
		strings.HasPrefix(target, "https://") ||
		isSSHRepository(target) ||
		strings.HasSuffix(target, ".git")
}

func isSSHRepository(target string) bool {
	return strings.HasPrefix(target, "ssh://") || isSCPLikeGitURL(target)
}

func isSCPLikeGitURL(target string) bool {
	return strings.Contains(target, "@") &&
		strings.Contains(target, ":") &&
		!strings.Contains(target, "://")
}

func authForRepository(repo string, insecureIgnoreHostKey bool) (transport.AuthMethod, error) {
	if !isSSHRepository(repo) {
		if insecureIgnoreHostKey {
			return nil, fmt.Errorf("--insecure-ignore-host-key requires an SSH repository URL")
		}
		return nil, nil
	}

	auth, err := gitssh.NewSSHAgentAuth(sshUserForRepository(repo))
	if err != nil {
		return nil, fmt.Errorf("ssh-agent auth setup failed: %w", err)
	}

	if insecureIgnoreHostKey {
		auth.HostKeyCallback = ssh.InsecureIgnoreHostKey()
		auth.HostKeyAlgorithms = nil
	}

	return auth, nil
}

func sshUserForRepository(repo string) string {
	if strings.HasPrefix(repo, "ssh://") {
		u, err := url.Parse(repo)
		if err == nil && u.User != nil {
			if username := u.User.Username(); username != "" {
				return username
			}
		}
		return "git"
	}

	if at := strings.Index(repo, "@"); at > 0 {
		return repo[:at]
	}

	return "git"
}

func exitIfErr(err error) {
	if err == nil {
		return
	}

	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
