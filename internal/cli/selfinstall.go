package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

func newSelfInstallCmd() *cobra.Command {
	var dir string
	var force bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Copy the running roksbnkargoctl binary into a directory on PATH",
		Long: `Copies the binary you are running into a directory on PATH, so
` + "`roksbnkargoctl`" + ` resolves from any working directory. This installs the tool
itself; ` + "`roksbnkargoctl install`" + ` installs BNK.

Default destination:
  Linux/macOS, in order of preference:
    $HOME/.local/bin   (usually writable without sudo)
    $HOME/bin
  Windows:
    a writable directory already on %PATH%, preferring
    %LOCALAPPDATA%\Microsoft\WindowsApps (on the per-user PATH by default,
    no admin), else %LOCALAPPDATA%\Programs\roksbnkargoctl with a PATH hint.

If the running binary already is the destination, this does nothing; --force
copies anyway. The one-line installers (install.sh, install.ps1) run
` + "`self install --force`" + ` from the binary they download.

Examples:
  roksbnkargoctl self install
  roksbnkargoctl self install --dir ~/bin
  sudo roksbnkargoctl self install --dir /usr/local/bin`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			self, err := runningBinary()
			if err != nil {
				return err
			}
			if dir == "" {
				dir = chooseInstallDir(runtime.GOOS)
			}
			_, err = installSelf(cmd.ErrOrStderr(), runtime.GOOS, self, dir, force)
			return err
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "destination directory (default: a PATH directory — ~/.local/bin on Linux/macOS, %LOCALAPPDATA%\\Microsoft\\WindowsApps on Windows)")
	cmd.Flags().BoolVar(&force, "force", false, "copy even when the destination is the running binary")
	return cmd
}

// installSelf copies self into destDir and returns the installed path.
func installSelf(w io.Writer, goos, self, destDir string, force bool) (string, error) {
	name := selfBinary
	if goos == "windows" {
		name += ".exe"
	}
	// A quoted --dir "~/bin" reaches us unexpanded.
	if home, err := os.UserHomeDir(); err == nil {
		if destDir == "~" {
			destDir = home
		} else if strings.HasPrefix(destDir, "~/") || strings.HasPrefix(destDir, `~\`) {
			destDir = filepath.Join(home, destDir[2:])
		}
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w (try --dir DIR, or sudo)", destDir, err)
	}
	dest := filepath.Join(destDir, name)

	if !force {
		absSelf, err1 := filepath.Abs(self)
		absDest, err2 := filepath.Abs(dest)
		if err1 == nil && err2 == nil && pathEqual(goos, absSelf, absDest) {
			fmt.Fprintf(w, "✓ Already installed at %s\n", dest)
			return dest, nil
		}
	}

	fmt.Fprintf(w, "→ Copying %s → %s\n", self, dest)
	if err := copyExecutable(goos, self, dest); err != nil {
		return "", fmt.Errorf("copying: %w (write permission? try --dir or sudo)", err)
	}
	fmt.Fprintf(w, "✓ Installed %s\n", dest)
	switch {
	case !isOnPATH(goos, destDir):
		printPATHGuidance(w, goos, destDir)
	case goos == "windows":
		fmt.Fprintln(w, "  (open a new terminal if `roksbnkargoctl` does not resolve immediately)")
	default:
		fmt.Fprintln(w, "  (open a new shell or run `hash -r` if `roksbnkargoctl` does not resolve immediately)")
	}
	return dest, nil
}

// copyExecutable stages src beside dest and moves it into place through the
// same path self update uses, so a dest that is a running .exe on Windows is
// moved aside rather than failing the copy.
func copyExecutable(goos, src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".tmp.*")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", filepath.Dir(dest), err)
	}
	staged := tmp.Name()
	defer func() { _ = os.Remove(staged) }() // a no-op once it has been renamed
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	// Close the source before any rename: with --force over the running binary,
	// src IS dest, and Windows refuses to rename a file that has an open handle
	// (found by the Windows CI job; Linux allows it, so no Linux test could).
	if err := in.Close(); err != nil {
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
	if _, err := os.Stat(dest); err != nil {
		// Nothing to move aside: a plain rename on every OS.
		return renameFile(staged, dest)
	}
	return installBinary(goos, dest, staged)
}

// printPATHGuidance says how to put dir on PATH, in the user's shell's terms:
// Unix `export PATH` advice is meaningless on Windows.
func printPATHGuidance(w io.Writer, goos, dir string) {
	fmt.Fprintf(w, "\nwarning: %s is not on your PATH\n", dir)
	if goos == "windows" {
		fmt.Fprintln(w, "  add it to your user PATH (new terminals pick it up):")
		fmt.Fprintf(w, "    [Environment]::SetEnvironmentVariable('Path', [Environment]::GetEnvironmentVariable('Path','User') + ';%s', 'User')\n", dir)
		fmt.Fprintln(w, "  then open a new terminal.")
		return
	}
	fmt.Fprintln(w, "  add this to your shell's rc file (~/.bashrc, ~/.zshrc, ...):")
	fmt.Fprintf(w, "    export PATH=\"%s:$PATH\"\n", dir)
	fmt.Fprintln(w, "  then `hash -r` or open a new shell.")
}

// chooseInstallDir prefers directories that need no sudo.
func chooseInstallDir(goos string) string {
	if goos == "windows" {
		return chooseInstallDirWindows()
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "/usr/local/bin"
	}
	local := filepath.Join(home, ".local", "bin")
	if isOnPATH(goos, local) || dirExists(local) {
		return local
	}
	if homeBin := filepath.Join(home, "bin"); isOnPATH(goos, homeBin) || dirExists(homeBin) {
		return homeBin
	}
	return local // created, with a PATH hint afterwards
}

// chooseInstallDirWindows picks a directory already on %PATH% so the binary
// resolves at once: WindowsApps (per-user PATH, no admin), else the first
// writable PATH entry under the profile, else a per-user directory we create.
func chooseInstallDirWindows() string {
	home, _ := os.UserHomeDir()
	local := os.Getenv("LOCALAPPDATA")
	if local != "" {
		if winApps := filepath.Join(local, "Microsoft", "WindowsApps"); isOnPATH("windows", winApps) && dirWritable(winApps) {
			return winApps
		}
	}
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if p != "" && isUnderDir("windows", p, home) && dirWritable(p) {
			return p
		}
	}
	if local != "" {
		return filepath.Join(local, "Programs", selfBinary)
	}
	if home != "" {
		return filepath.Join(home, "."+selfBinary, "bin")
	}
	return "."
}

// isOnPATH reports whether dir is a PATH entry, comparing absolute paths
// (case-insensitively on Windows).
func isOnPATH(goos, dir string) bool {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if p == "" {
			continue
		}
		if pAbs, err := filepath.Abs(p); err == nil && pathEqual(goos, pAbs, abs) {
			return true
		}
	}
	return false
}

func pathEqual(goos, a, b string) bool {
	if goos == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// isUnderDir reports whether path is dir or below it.
func isUnderDir(goos, path, dir string) bool {
	if dir == "" {
		return false
	}
	absP, err1 := filepath.Abs(path)
	absD, err2 := filepath.Abs(dir)
	if err1 != nil || err2 != nil {
		return false
	}
	if pathEqual(goos, absP, absD) {
		return true
	}
	prefix := absD + string(os.PathSeparator)
	if goos == "windows" {
		return strings.HasPrefix(strings.ToLower(absP), strings.ToLower(prefix))
	}
	return strings.HasPrefix(absP, prefix)
}

func dirExists(d string) bool {
	info, err := os.Stat(d)
	return err == nil && info.IsDir()
}

// dirWritable probes an existing directory by creating and removing a file;
// Windows ACLs make a stat-based guess unreliable. It never creates dir.
func dirWritable(dir string) bool {
	if !dirExists(dir) {
		return false
	}
	f, err := os.CreateTemp(dir, "."+selfBinary+"-wtest-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}
