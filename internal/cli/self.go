package cli

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
)

const (
	selfRepo          = "jgruberf5/roksbnkargoctl"
	selfBinary        = "roksbnkargoctl"
	selfUpdateTimeout = 5 * time.Minute
	// maxDownload bounds a release download held in memory. A release archive
	// is ~60 MB; anything near this is not a roksbnkargoctl archive.
	maxDownload = 512 << 20
	// exitSelfUpdateStranded is returned when a replace removed the old binary
	// and could not put it back, so a human has to rename the sidecar. It is
	// distinct from 1 so a wrapper can tell "retry" from "nothing left to retry".
	exitSelfUpdateStranded = 125
)

// githubAPI is the GitHub REST base. A variable so tests can point the whole
// pipeline, API and downloads alike, at an httptest server.
var githubAPI = "https://api.github.com"

func newSelfCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "self",
		Short: "Manage the roksbnkargoctl binary itself (install onto PATH, update)",
	}
	cmd.AddCommand(newSelfInstallCmd(), newSelfUpdateCmd())
	return cmd
}

// ---- self update ------------------------------------------------------------

func newSelfUpdateCmd() *cobra.Command {
	var pinned string
	var check bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update this binary in place from a GitHub release for the same BNK version",
		Long: `Downloads a roksbnkargoctl release for this OS/arch from GitHub, verifies its
SHA256 against the release's checksums file, and replaces the running binary.

Each binary installs one BNK release (` + "`roksbnkargoctl version`" + ` names it), and the
release archives are named for it:
  roksbnkargoctl_<version>_bnk-<BNK version>_<os>_<arch>.tar.gz   (.zip on Windows)
self update only ever installs the archive for this binary's own BNK version. It
never switches BNK lines: a release without that archive is skipped when looking
for the latest, and refused when named with --version.

  self update                 on a terminal: list the newer releases, pick one
                              without a terminal, or with --yes: the latest
  self update --version vX.Y.Z  install that release (may downgrade or reinstall)
  self update --check         report what is available and change nothing

On Windows the running .exe cannot be overwritten, so it is moved aside to
<binary>.old and the new binary takes its place; the .old file is removed on the
next run.

Needs write permission on the binary's directory. GITHUB_TOKEN, when set,
authenticates the GitHub API calls (60 requests/hour without it).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), selfUpdateTimeout)
			defer cancel()
			u := newUpdater(cmd.ErrOrStderr())
			if check {
				return u.check(ctx, cmd.OutOrStdout())
			}
			target, err := runningBinary()
			if err != nil {
				return err
			}
			return u.run(ctx, target, pinned, flagYes || !isTTY())
		},
	}
	cmd.Flags().StringVar(&pinned, "version", "", "release to install, e.g. v0.5.1 (default: the latest release for this BNK version)")
	cmd.Flags().BoolVar(&check, "check", false, "report the current and available releases for this BNK version, and change nothing")
	return cmd
}

// updater is the self-update pipeline. Every host-specific input is a field
// so a test can drive the Windows zip path, another BNK line, or a fake GitHub
// from any host.
type updater struct {
	api     string // GitHub REST base
	repo    string
	client  *http.Client
	goos    string
	goarch  string
	bnk     string // the BNK version this binary installs; never changes
	current string // this binary's version
	w       io.Writer
}

func newUpdater(w io.Writer) *updater {
	return &updater{
		api: githubAPI, repo: selfRepo, client: http.DefaultClient,
		goos: runtime.GOOS, goarch: runtime.GOARCH,
		bnk: config.BNKVersion, current: Version, w: w,
	}
}

// run resolves a release, downloads and verifies its archive for this BNK
// version, and replaces target. auto skips the picker and the confirmation.
func (u *updater) run(ctx context.Context, target, pinned string, auto bool) error {
	pinned = strings.TrimSpace(pinned)
	var rel *ghRelease
	switch {
	case pinned != "":
		tag := normalizeTag(pinned)
		fmt.Fprintf(u.w, "→ Fetching release %s\n", tag)
		r, err := u.releaseByTag(ctx, tag)
		if err != nil {
			return err
		}
		rel = r
	case auto:
		fmt.Fprintf(u.w, "→ Checking for the latest release for BNK %s\n", u.bnk)
		r, err := u.latest(ctx)
		if err != nil {
			return err
		}
		rel = r
	default:
		r, err := u.pick(ctx)
		if err != nil {
			return err
		}
		if r == nil {
			return nil
		}
		rel = r
	}

	fmt.Fprintf(u.w, "  Current: %s (BNK %s)\n", u.current, u.bnk)
	fmt.Fprintf(u.w, "  Target:  %s (BNK %s)\n", rel.TagName, u.bnk)
	switch {
	case pinned == "" && sameVersion(u.current, rel.TagName):
		fmt.Fprintf(u.w, "✓ Already at the latest release for BNK %s\n", u.bnk)
		return nil
	case sameVersion(u.current, rel.TagName):
		fmt.Fprintf(u.w, "  (reinstalling %s)\n", rel.TagName)
	case u.current == "dev":
		fmt.Fprintln(u.w, "  (current is a dev build)")
	}
	if !auto && !askYesNo(fmt.Sprintf("Switch to %s?", rel.TagName), true) {
		return errors.New("aborted")
	}

	bin, err := u.fetchBinary(ctx, rel)
	if err != nil {
		return err
	}
	fmt.Fprintf(u.w, "→ Replacing %s\n", target)
	if err := replaceBinary(u.goos, target, bin); err != nil {
		var stranded errRollbackFailed
		if errors.As(err, &stranded) {
			return exitError{code: exitSelfUpdateStranded, err: err}
		}
		return fmt.Errorf("replacing binary: %w", err)
	}
	fmt.Fprintf(u.w, "✓ Now at %s (BNK %s)\n", rel.TagName, u.bnk)
	if u.goos == "windows" {
		fmt.Fprintf(u.w, "  (previous binary moved to %s.old — removed on the next run)\n", target)
	}
	return nil
}

// check reports, without changing anything, the releases newer than this one
// that carry an archive for this binary's BNK version.
func (u *updater) check(ctx context.Context, out io.Writer) error {
	rels, err := u.eligible(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Current: %s (BNK %s, %s/%s)\n", u.current, u.bnk, u.goos, u.goarch)
	if len(rels) == 0 {
		fmt.Fprintf(out, "No release of %s has an archive for BNK %s on %s/%s yet.\n", u.repo, u.bnk, u.goos, u.goarch)
		return nil
	}
	newer := u.newerThanCurrent(rels)
	if len(newer) == 0 {
		fmt.Fprintf(out, "Up to date: %s is the latest release for BNK %s.\n", rels[0].TagName, u.bnk)
		return nil
	}
	fmt.Fprintf(out, "Newer releases for BNK %s:\n", u.bnk)
	for i, r := range newer {
		suffix := ""
		if i == 0 {
			suffix = "  (latest)"
		}
		fmt.Fprintf(out, "  %s%s\n", r.TagName, suffix)
	}
	fmt.Fprintln(out, "Run `roksbnkargoctl self update` to install one.")
	return nil
}

// pick lists the releases newer than this binary (for its BNK version) and
// lets the operator choose. nil means nothing newer, or cancelled.
func (u *updater) pick(ctx context.Context) (*ghRelease, error) {
	rels, err := u.eligible(ctx)
	if err != nil {
		return nil, err
	}
	if len(rels) == 0 {
		return nil, fmt.Errorf("no release of %s has an archive for BNK %s on %s/%s", u.repo, u.bnk, u.goos, u.goarch)
	}
	newer := u.newerThanCurrent(rels)
	if len(newer) == 0 {
		fmt.Fprintf(u.w, "✓ %s is the latest release for BNK %s\n", u.current, u.bnk)
		return nil, nil
	}
	tags := make([]string, len(newer))
	for i, r := range newer {
		tags[i] = r.TagName
	}
	fmt.Fprintf(u.w, "Current version: %s (BNK %s)\n", u.current, u.bnk)
	chosen := choose("Newer releases for BNK "+u.bnk, tags, tags[0])
	for i := range newer {
		if newer[i].TagName == chosen {
			return &newer[i], nil
		}
	}
	return nil, nil
}

// fetchBinary downloads this BNK version's archive from rel, verifies it
// against the release's checksums file, and returns the extracted binary.
func (u *updater) fetchBinary(ctx context.Context, rel *ghRelease) ([]byte, error) {
	asset, err := u.assetFor(rel)
	if err != nil {
		return nil, err
	}
	sumsName := checksumsName(rel.TagName)
	sums, ok := findAsset(rel.Assets, sumsName)
	if !ok {
		return nil, fmt.Errorf("release %s has no %s; refusing to update without checksum verification", rel.TagName, sumsName)
	}

	fmt.Fprintf(u.w, "→ Downloading %s (%d bytes)\n", asset.Name, asset.Size)
	archive, err := u.download(ctx, asset.URL)
	if err != nil {
		return nil, err
	}
	fmt.Fprintln(u.w, "→ Verifying checksum")
	sumsBody, err := u.download(ctx, sums.URL)
	if err != nil {
		return nil, err
	}
	expected, err := checksumFor(sumsBody, sumsName, asset.Name)
	if err != nil {
		return nil, err
	}
	if actual := sha256Hex(archive); !strings.EqualFold(actual, expected) {
		return nil, fmt.Errorf("checksum mismatch for %s: expected %s, got %s", asset.Name, expected, actual)
	}
	fmt.Fprintln(u.w, "→ Extracting binary")
	return extractBinary(u.goos, archive)
}

// assetFor returns rel's archive for this binary's BNK version, OS and arch.
// When there is none it says which BNK versions rel does carry, because the
// likely cause is a release cut for another BNK line, and this binary must not
// quietly start installing that one.
func (u *updater) assetFor(rel *ghRelease) (ghAsset, error) {
	name := assetName(rel.TagName, u.bnk, u.goos, u.goarch)
	if a, ok := findAsset(rel.Assets, name); ok {
		return a, nil
	}
	lines := bnkVersionsIn(rel)
	for _, l := range lines {
		if l == u.bnk {
			return ghAsset{}, fmt.Errorf("release %s has BNK %s archives, but none for %s/%s (wanted %s)",
				rel.TagName, u.bnk, u.goos, u.goarch, name)
		}
	}
	has := "no BNK archives at all"
	if len(lines) > 0 {
		has = "archives for BNK " + strings.Join(lines, ", ")
	}
	return ghAsset{}, fmt.Errorf("release %s has no archive for BNK %s (this binary's BNK version); it has %s. "+
		"self update never switches BNK lines — download a binary for the other line from https://github.com/%s/releases/tag/%s",
		rel.TagName, u.bnk, has, u.repo, rel.TagName)
}

// latest returns the newest non-draft, non-prerelease release that has an
// archive for this BNK version, OS and arch. A newer release without one (a
// release cut for another BNK line) is skipped rather than treated as latest.
func (u *updater) latest(ctx context.Context) (*ghRelease, error) {
	rels, err := u.eligible(ctx)
	if err != nil {
		return nil, err
	}
	if len(rels) == 0 {
		return nil, fmt.Errorf("no release of %s has an archive for BNK %s on %s/%s", u.repo, u.bnk, u.goos, u.goarch)
	}
	return &rels[0], nil
}

// newerThanCurrent keeps the releases strictly newer than this binary, in
// order; all of them when this binary is not a release build (e.g. "dev").
func (u *updater) newerThanCurrent(rels []ghRelease) []ghRelease {
	cur, ok := parseSemver(u.current)
	if !ok {
		return rels
	}
	var out []ghRelease
	for _, r := range rels {
		if v, _ := parseSemver(r.TagName); compareSemver(v, cur) > 0 {
			out = append(out, r)
		}
	}
	return out
}

// eligible lists published releases with an archive for this BNK version, OS
// and arch, newest version first. Tags that are not vMAJOR.MINOR.PATCH are
// ignored, as are drafts and prereleases (--version installs those).
func (u *updater) eligible(ctx context.Context) ([]ghRelease, error) {
	var rels []ghRelease
	url := fmt.Sprintf("%s/repos/%s/releases?per_page=100", u.api, u.repo)
	if err := u.getJSON(ctx, url, fmt.Sprintf("no releases for %s", u.repo), &rels); err != nil {
		return nil, err
	}
	type vrel struct {
		v semver
		r ghRelease
	}
	var ok []vrel
	for _, r := range rels {
		if r.Draft || r.Prerelease {
			continue
		}
		v, parsed := parseSemver(r.TagName)
		if !parsed || v.pre != "" {
			continue
		}
		if _, has := findAsset(r.Assets, assetName(r.TagName, u.bnk, u.goos, u.goarch)); !has {
			continue
		}
		ok = append(ok, vrel{v, r})
	}
	sort.SliceStable(ok, func(i, j int) bool { return compareSemver(ok[i].v, ok[j].v) > 0 })
	out := make([]ghRelease, len(ok))
	for i := range ok {
		out[i] = ok[i].r
	}
	return out, nil
}

func (u *updater) releaseByTag(ctx context.Context, tag string) (*ghRelease, error) {
	var r ghRelease
	url := fmt.Sprintf("%s/repos/%s/releases/tags/%s", u.api, u.repo, tag)
	if err := u.getJSON(ctx, url, fmt.Sprintf("release %s not found in %s", tag, u.repo), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// getJSON GETs a GitHub API endpoint. notFound is returned verbatim on a 404,
// and an exhausted rate limit says so and names the fix, rather than surfacing
// a bare "403 Forbidden".
func (u *updater) getJSON(ctx context.Context, url, notFound string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", selfBinary)
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return fmt.Errorf("github request: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return errors.New(notFound)
	case http.StatusForbidden, http.StatusTooManyRequests:
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return errors.New("github rate limit exhausted; set GITHUB_TOKEN to authenticate")
		}
		return fmt.Errorf("github returned %s for %s", resp.Status, url)
	default:
		return fmt.Errorf("github returned %s for %s", resp.Status, url)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("decoding github response: %w", err)
	}
	return nil
}

// download fetches a release asset. GITHUB_TOKEN is deliberately not sent:
// asset URLs redirect to a storage host that must not receive it.
func (u *updater) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", selfBinary)
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxDownload {
		return nil, fmt.Errorf("downloading %s: larger than %d bytes", url, maxDownload)
	}
	return b, nil
}

// ---- release naming -----------------------------------------------------------

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

type ghRelease struct {
	TagName    string    `json:"tag_name"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Assets     []ghAsset `json:"assets"`
}

// assetName is .goreleaser.yaml's archive name_template:
// "{{ .ProjectName }}_{{ .Version }}_bnk-{{ .Env.BNK_VERSION }}_{{ .Os }}_{{ .Arch }}"
// plus .tar.gz, or .zip on Windows. .Version is the tag without its leading v.
func assetName(tag, bnk, goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("%s_%s_bnk-%s_%s_%s%s", selfBinary, strings.TrimPrefix(tag, "v"), bnk, goos, goarch, ext)
}

// checksumsName is .goreleaser.yaml's checksum name_template.
func checksumsName(tag string) string {
	return fmt.Sprintf("%s_%s_checksums.txt", selfBinary, strings.TrimPrefix(tag, "v"))
}

var bnkAssetRE = regexp.MustCompile(`^` + selfBinary + `_[^_]+_bnk-([^_]+)_[^_]+_[^_]+\.(?:tar\.gz|zip)$`)

// bnkVersionsIn lists the BNK versions rel has archives for, sorted.
func bnkVersionsIn(rel *ghRelease) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range rel.Assets {
		if m := bnkAssetRE.FindStringSubmatch(a.Name); m != nil && !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

func findAsset(assets []ghAsset, name string) (ghAsset, bool) {
	for _, a := range assets {
		if a.Name == name {
			return a, true
		}
	}
	return ghAsset{}, false
}

// normalizeTag ensures a leading v, so "0.5.1" and "v0.5.1" both name the tag.
func normalizeTag(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}

// sameVersion compares versions ignoring a leading v: tags carry it, and a
// version stamped without it must still match its own release.
func sameVersion(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}

type semver struct {
	major, minor, patch int
	pre                 string
}

// parseSemver parses [v]MAJOR.MINOR.PATCH[-PRE][+BUILD].
func parseSemver(s string) (semver, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var v semver
	if i := strings.IndexByte(s, '-'); i >= 0 {
		v.pre, s = s[i+1:], s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	nums := [3]*int{&v.major, &v.minor, &v.patch}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, false
		}
		*nums[i] = n
	}
	return v, true
}

// compareSemver orders by MAJOR.MINOR.PATCH, and a prerelease below its release.
func compareSemver(a, b semver) int {
	for _, d := range []int{a.major - b.major, a.minor - b.minor, a.patch - b.patch} {
		if d != 0 {
			if d > 0 {
				return 1
			}
			return -1
		}
	}
	switch {
	case a.pre == b.pre:
		return 0
	case a.pre == "":
		return 1
	case b.pre == "":
		return -1
	}
	return strings.Compare(a.pre, b.pre)
}

// ---- verification + extraction ---------------------------------------------------

// checksumFor returns filename's SHA256 from a goreleaser checksums file
// ("<sha256>  <name>" per line).
func checksumFor(sums []byte, sumsName, filename string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) >= 2 && parts[1] == filename {
			return parts[0], nil
		}
	}
	return "", fmt.Errorf("%s does not list %s; refusing to install an unverified archive", sumsName, filename)
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// extractBinary pulls the binary out of a release archive: a .tar.gz holding
// roksbnkargoctl, or on Windows a .zip holding roksbnkargoctl.exe, beside
// LICENSE, README.md and CHANGELOG.md.
func extractBinary(goos string, archive []byte) ([]byte, error) {
	if goos == "windows" {
		return extractFromZip(archive, selfBinary+".exe")
	}
	return extractFromTarGz(archive, selfBinary)
}

func extractFromTarGz(archive []byte, want string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar reader: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != want {
			continue
		}
		return io.ReadAll(io.LimitReader(tr, maxDownload))
	}
	return nil, fmt.Errorf("%s not found in the archive", want)
}

func extractFromZip(archive []byte, want string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("zip reader: %w", err)
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || filepath.Base(f.Name) != want {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("opening %s in zip: %w", f.Name, err)
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, maxDownload))
	}
	return nil, fmt.Errorf("%s not found in the archive", want)
}

// ---- atomic replace + rollback ----------------------------------------------------

// runningBinary is the path of this executable, symlinks resolved, so a
// replace lands on the real file and not on a link to it.
func runningBinary() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolving running binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	return self, nil
}

// replaceBinary stages newBinary beside target (same directory, because a
// rename is atomic only within one filesystem) and installs it over target.
func replaceBinary(goos, target string, newBinary []byte) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(target)+".tmp.*")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w (write permission?)", dir, err)
	}
	staged := tmp.Name()
	installed := false
	defer func() {
		if !installed {
			_ = os.Remove(staged)
		}
	}()
	if _, err := tmp.Write(newBinary); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := installBinary(goos, target, staged); err != nil {
		return err
	}
	installed = true
	return nil
}

// installBinary moves staged onto target. On Unix the running binary is an
// open inode, so a rename over it is atomic and safe. Windows locks a running
// .exe against overwrite, so it takes the move-aside path.
func installBinary(goos, target, staged string) error {
	if goos == "windows" {
		return installByMoveAside(target, staged)
	}
	if err := renameFile(staged, target); err != nil {
		return fmt.Errorf("renaming onto %s: %w (write permission? try sudo, or --dir)", target, err)
	}
	return nil
}

// renameFile indirects os.Rename so the unrecoverable rollback branch can be
// driven in a test: target and target.old share a directory, so no ordinary
// permission change can fail the rollback without failing the first rename.
// Tests that stub it must not run in parallel.
var renameFile = os.Rename

// installByMoveAside is the Windows-safe replace: a running .exe cannot be
// overwritten but can be renamed. Move target to target.old, then the staged
// binary into target's place. On failure the move is rolled back; if the
// rollback itself fails there is no binary at target, and the error says so
// and gives the command that recovers it (errRollbackFailed).
func installByMoveAside(target, staged string) error {
	old := target + ".old"
	_ = os.Remove(old) // a stale sidecar from an earlier update
	if err := renameFile(target, old); err != nil {
		return fmt.Errorf("moving current binary aside: %w", err)
	}
	if err := renameFile(staged, target); err != nil {
		if rerr := renameFile(old, target); rerr != nil {
			return errRollbackFailed{install: err, rollback: rerr, old: old, target: target}
		}
		return fmt.Errorf("installing new binary: %w", err)
	}
	_ = os.Remove(old) // fails while the old process runs; swept on the next start
	return nil
}

// errRollbackFailed is the one state installByMoveAside cannot recover from:
// the new binary would not move in AND the old one would not move back. A type,
// so both causes stay visible to errors.Is/As and the path can be asserted
// without matching prose.
type errRollbackFailed struct {
	install, rollback error
	old, target       string
}

func (e errRollbackFailed) Error() string {
	return fmt.Sprintf(
		"installing new binary: %v; rolling back also failed: %v\n"+
			"There is now NO binary at %s. Your previous one is intact at %s — "+
			"rename it back to recover:\n    %s",
		e.install, e.rollback, e.target, e.old, recoverCommand(runtime.GOOS, e.old, e.target))
}

func (e errRollbackFailed) Unwrap() []error { return []error{e.install, e.rollback} }

// recoverCommand is the shell command that puts the old binary back. The
// move-aside path only runs on Windows, where cmd.exe has no mv, so it is
// Move-Item there. Paths are quoted literally, not with %q, which would double
// every backslash and name a path that does not exist. goos is a parameter so
// the Windows branch is tested on the Linux and macOS CI runners.
func recoverCommand(goos, old, target string) string {
	if goos == "windows" {
		return fmt.Sprintf("Move-Item -LiteralPath \"%s\" -Destination \"%s\"", old, target)
	}
	return fmt.Sprintf("mv \"%s\" \"%s\"", old, target)
}

// sweepStaleBinary removes a <self>.old left by an earlier Windows update, once
// the process that held it is gone. Best-effort; a no-op off Windows.
func sweepStaleBinary() {
	if runtime.GOOS != "windows" {
		return
	}
	if self, err := runningBinary(); err == nil {
		_ = os.Remove(self + ".old")
	}
}
