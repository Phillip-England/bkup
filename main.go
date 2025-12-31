// bkup: simple cross-platform directory backup with versioning + go/revert helpers.
//
// Backup root:
//   $HOME/.bkup   (works on macOS/Linux/Windows via os.UserHomeDir)
//
// Layout:
//   $HOME/.bkup/config.json
//   $HOME/.bkup/<project>_backup/<project>_0
//   $HOME/.bkup/<project>_backup/<project>_1
//   ...
//
// Usage:
//   bkup [-q]                # create a new versioned backup of current dir
//   bkup go [--print]        # ALWAYS go to the newest version (does NOT create a new backup)
//   bkup revert [--print]    # subshell into saved "prev" location
//   bkup list                # list backups for current project
//   bkup pull <number> [-q]  # safety-backup current dir, then replace current dir contents with backup <number>
//   bkup clean               # delete backups for current project
//   bkup cleanse             # delete all project backups under ~/.bkup, keep config.json
//   bkup config              # open ~/.bkup/config.json in $EDITOR (or vi / notepad)
//
// Config (JSON):
// {
//   "max_versions": 10,
//   "prev_path": "/path/you/came/from"
// }
//
// Capacity behavior:
// - Default (no -q): HARD CAP. If max_versions is reached, operations that need a NEW backup refuse.
// - Queue mode (-q): FIFO. If max_versions is reached, the oldest slot is overwritten to make room.
// - IMPORTANT: if max_versions is 10, backup directories will ALWAYS be numbered 0..9 (never higher).
//
// Newest/oldest selection:
// - Determined by a per-backup metadata file: <backup>/.bkup_meta.json (created_unix timestamp).
// - This makes "newest" deterministic even when -q overwrites slots.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	backupFolderName = ".bkup"
	configFileName   = "config.json"
	metaFileName     = ".bkup_meta.json"
)

type Config struct {
	MaxVersions int    `json:"max_versions"`
	PrevPath    string `json:"prev_path"`
}

type Meta struct {
	CreatedUnix int64  `json:"created_unix"`
	CreatedRFC  string `json:"created_rfc3339"`
}

func main() {
	args := os.Args[1:]

	printMode := false
	queueMode := false

	// Strip flags anywhere: --print and -q
	filtered := make([]string, 0, len(args))
	for _, a := range args {
		switch a {
		case "--print":
			printMode = true
			continue
		case "-q":
			queueMode = true
			continue
		default:
			filtered = append(filtered, a)
		}
	}
	args = filtered

	backupRoot, err := getBackupRoot()
	if err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(backupRoot, 0o755); err != nil {
		fatal(fmt.Errorf("create backup root: %w", err))
	}

	cfgPath := filepath.Join(backupRoot, configFileName)
	cfg, err := loadOrInitConfig(cfgPath)
	if err != nil {
		fatal(err)
	}

	switch {
	case len(args) == 0:
		// bkup [-q]
		cwd, err := os.Getwd()
		if err != nil {
			fatal(err)
		}
		dst, err := backupNewVersion(cwd, backupRoot, cfg, queueMode, nil)
		if err != nil {
			fatal(err)
		}
		fmt.Println(dst)

	case args[0] == "config":
		// bkup config
		if err := ensureConfigExists(cfgPath, cfg); err != nil {
			fatal(err)
		}
		if err := openEditor(cfgPath); err != nil {
			fatal(err)
		}

	case args[0] == "go":
		// bkup go [--print]
		//
		// Per your spec: go does NOT create a new backup (except first-run where none exist).
		// It jumps to the newest existing backup, determined by .bkup_meta.json timestamps.
		cwd, err := os.Getwd()
		if err != nil {
			fatal(err)
		}
		cwdAbs := mustAbs(cwd)
		project := filepath.Base(cwdAbs)
		projectRoot := filepath.Join(backupRoot, project+"_backup")

		latest, err := newestBackupPath(projectRoot, project)
		if err != nil {
			fatal(err)
		}
		if latest == "" {
			created, err := backupNewVersion(cwdAbs, backupRoot, cfg, queueMode, nil)
			if err != nil {
				fatal(err)
			}
			latest = created
		}

		// Save previous location in config.
		cfg.PrevPath = cwdAbs
		if err := saveConfigAtomic(cfgPath, cfg); err != nil {
			fatal(err)
		}

		if printMode {
			fmt.Println(latest)
			return
		}
		if err := openSubshell(latest); err != nil {
			fatal(err)
		}

	case args[0] == "revert":
		// bkup revert [--print]
		cfg, err := loadOrInitConfig(cfgPath)
		if err != nil {
			fatal(err)
		}
		if strings.TrimSpace(cfg.PrevPath) == "" {
			fatal(fmt.Errorf("prev_path is empty in %s (run `bkup go` first)", cfgPath))
		}

		if printMode {
			fmt.Println(cfg.PrevPath)
			return
		}
		if err := openSubshell(cfg.PrevPath); err != nil {
			fatal(err)
		}

	case args[0] == "list":
		// bkup list
		cwd, err := os.Getwd()
		if err != nil {
			fatal(err)
		}
		project := filepath.Base(mustAbs(cwd))
		projectRoot := filepath.Join(backupRoot, project+"_backup")

		vers, err := listProjectVersions(projectRoot, project)
		if err != nil {
			fatal(err)
		}
		if len(vers) == 0 {
			fmt.Println("(no backups found)")
			return
		}
		sort.Slice(vers, func(i, j int) bool { return vers[i].N < vers[j].N })
		for _, v := range vers {
			fmt.Println(v.Path)
		}

	case args[0] == "pull":
		// bkup pull <number> [-q]
		if len(args) < 2 {
			fatal(errors.New("usage: bkup pull <number> [-q]"))
		}
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 0 {
			fatal(fmt.Errorf("invalid backup number: %q", args[1]))
		}

		cwd, err := os.Getwd()
		if err != nil {
			fatal(err)
		}
		cwdAbs := mustAbs(cwd)
		project := filepath.Base(cwdAbs)
		projectRoot := filepath.Join(backupRoot, project+"_backup")
		pullSrc := filepath.Join(projectRoot, fmt.Sprintf("%s_%d", project, n))

		// Ensure requested backup exists BEFORE doing anything else.
		if fi, err := os.Stat(pullSrc); err != nil || !fi.IsDir() {
			if err != nil {
				fatal(fmt.Errorf("backup not found: %s (%w)", pullSrc, err))
			}
			fatal(fmt.Errorf("backup not found (not a directory): %s", pullSrc))
		}

		// Never overwrite the backup we're pulling FROM.
		protected := map[int]bool{n: true}

		// Create safety backup first (hard-cap may refuse; -q may overwrite oldest excluding protected).
		safetyDst, err := backupNewVersion(cwdAbs, backupRoot, cfg, queueMode, protected)
		if err != nil {
			fatal(fmt.Errorf("refusing to pull because a safety backup cannot be created first: %w", err))
		}

		// Replace current directory contents with the pulled backup.
		if err := replaceDirContents(cwdAbs, pullSrc); err != nil {
			fatal(err)
		}

		fmt.Printf("Pulled %s into %s\n", pullSrc, cwdAbs)
		fmt.Printf("Safety backup created: %s\n", safetyDst)

	case args[0] == "clean":
		// bkup clean (single project)
		cwd, err := os.Getwd()
		if err != nil {
			fatal(err)
		}
		project := filepath.Base(mustAbs(cwd))
		projectRoot := filepath.Join(backupRoot, project+"_backup")

		if err := os.RemoveAll(projectRoot); err != nil {
			fatal(fmt.Errorf("remove project backups: %w", err))
		}
		fmt.Println("Removed:", projectRoot)

	case args[0] == "cleanse":
		// bkup cleanse (entire backup root except config.json)
		removed, err := cleanseBackupRoot(backupRoot, cfgPath)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("Cleansed %d item(s). Kept %s.\n", removed, cfgPath)

	case len(args) >= 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help"):
		usage()

	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println(`bkup - versioned directory backups into a cross-platform backup location

Usage:
  bkup [-q]
      Create a new versioned backup of the current directory:
      $HOME/.bkup/<dirname>_backup/<dirname>_<N>

  bkup go [--print]
      Go to the newest existing backup for the current project (does NOT create a new backup).
      If no backups exist yet, it creates the first one.
      "Newest" is determined by <backup>/.bkup_meta.json timestamps.
      With --print: just print the newest backup directory path.

  bkup revert [--print]
      Open a subshell in prev_path stored in config.json.
      With --print: just print the prev_path.

  bkup list
      List all backups for the current project.

  bkup pull <number> [-q]
      Safety-backup the current directory (so you can undo), then replace the current
      directory contents with the chosen backup version. Your current path stays the same.
      - Default: refuses if max_versions is reached (to avoid data loss).
      - With -q: overwrites the oldest backup (FIFO) to make room.

  bkup clean
      Delete all backups for the current project only.

  bkup cleanse
      Delete everything under $HOME/.bkup except config.json.

  bkup config
      Open $HOME/.bkup/config.json in $EDITOR (or vi / notepad).

Queue mode (-q):
  Treat backups like a FIFO queue. When max_versions is reached, the oldest backup
  is overwritten to allow creating a new backup.

Numbering rule:
  If max_versions is 10, backups are always numbered 0..9 (never higher).
`)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "bkup error:", err)
	os.Exit(1)
}

func getBackupRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("could not determine home directory")
	}
	return filepath.Join(home, backupFolderName), nil
}

func mustAbs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

// -------------------- CONFIG --------------------

func loadOrInitConfig(cfgPath string) (Config, error) {
	def := Config{
		MaxVersions: 10,
		PrevPath:    "",
	}

	b, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err := saveConfigAtomic(cfgPath, def); err != nil {
				return Config{}, err
			}
			return def, nil
		}
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", cfgPath, err)
	}

	if cfg.MaxVersions <= 0 {
		cfg.MaxVersions = def.MaxVersions
	}
	return cfg, nil
}

func ensureConfigExists(cfgPath string, cfg Config) error {
	_, err := os.Stat(cfgPath)
	if err == nil {
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	return saveConfigAtomic(cfgPath, cfg)
}

func saveConfigAtomic(cfgPath string, cfg Config) error {
	tmp := cfgPath + ".tmp"
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	b = append(b, '\n')

	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("write temp config: %w", err)
	}
	return os.Rename(tmp, cfgPath)
}

// -------------------- META --------------------

func metaPathForDir(backupDir string) string {
	return filepath.Join(backupDir, metaFileName)
}

func writeMetaAtomic(backupDir string, created time.Time) error {
	m := Meta{
		CreatedUnix: created.Unix(),
		CreatedRFC:  created.UTC().Format(time.RFC3339),
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal meta: %w", err)
	}
	b = append(b, '\n')

	p := metaPathForDir(backupDir)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("write meta temp: %w", err)
	}
	return os.Rename(tmp, p)
}

// readCreatedUnix reads .bkup_meta.json created_unix.
// Returns (createdUnix, true, nil) if present.
// If missing/unreadable, returns (fallback, false, nil) where fallback is dir modtime unix if stat succeeds.
func readCreatedUnix(backupDir string) (int64, bool, error) {
	p := metaPathForDir(backupDir)
	b, err := os.ReadFile(p)
	if err == nil {
		var m Meta
		if err := json.Unmarshal(b, &m); err != nil {
			return 0, false, fmt.Errorf("parse meta %s: %w", p, err)
		}
		if m.CreatedUnix > 0 {
			return m.CreatedUnix, true, nil
		}
	}

	// fallback to directory modtime
	fi, statErr := os.Stat(backupDir)
	if statErr == nil {
		return fi.ModTime().Unix(), false, nil
	}
	// if both fail, treat as 0
	return 0, false, nil
}

// -------------------- BACKUP LOGIC --------------------

type Version struct {
	N           int
	Path        string
	CreatedUnix int64 // from .bkup_meta.json (preferred), else dir modtime unix
	HasMeta     bool
}

// backupNewVersion creates a new backup version.
//
// Numbering rule when MaxVersions > 0:
//   - Directories are ALWAYS in the range 0..MaxVersions-1.
//   - No "-q": if all slots are taken, refuse.
//   - "-q": overwrite the oldest slot (FIFO) to make room (excluding protectedNums).
//
// protectedNums (optional) prevents overwriting certain slot numbers.
func backupNewVersion(srcAbs string, backupRoot string, cfg Config, queueMode bool, protectedNums map[int]bool) (string, error) {
	srcAbs = mustAbs(srcAbs)
	project := filepath.Base(srcAbs)

	projectRoot := filepath.Join(backupRoot, project+"_backup")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		return "", fmt.Errorf("create project root: %w", err)
	}

	vers, err := listProjectVersions(projectRoot, project)
	if err != nil {
		return "", err
	}

	// Unlimited mode (MaxVersions <= 0): keep growing (legacy behavior).
	if cfg.MaxVersions <= 0 {
		next := 0
		if len(vers) > 0 {
			sort.Slice(vers, func(i, j int) bool { return vers[i].N < vers[j].N })
			next = vers[len(vers)-1].N + 1
		}
		dst := filepath.Join(projectRoot, fmt.Sprintf("%s_%d", project, next))
		_ = os.RemoveAll(dst)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return "", fmt.Errorf("create dest: %w", err)
		}
		if err := copyDirContents(srcAbs, dst); err != nil {
			_ = os.RemoveAll(dst)
			return "", err
		}
		if err := writeMetaAtomic(dst, time.Now()); err != nil {
			_ = os.RemoveAll(dst)
			return "", err
		}
		return dst, nil
	}

	max := cfg.MaxVersions

	// Build set of used slots (only 0..max-1).
	used := make(map[int]Version, len(vers))
	for _, v := range vers {
		if v.N < 0 || v.N >= max {
			continue
		}
		used[v.N] = v
	}

	// Pick a slot.
	slot := -1

	// If there is a free slot, pick smallest free.
	if len(used) < max {
		for i := 0; i < max; i++ {
			if _, ok := used[i]; !ok {
				slot = i
				break
			}
		}
	} else {
		// Full
		if !queueMode {
			return "", fmt.Errorf(
				"max_versions reached (%d) for project %q; refusing to create a new backup. "+
					"Use -q to enable FIFO overwrite, increase max_versions in %s, or run `bkup clean`.",
				max, project, filepath.Join(backupRoot, configFileName),
			)
		}

		// Queue mode: overwrite oldest by CreatedUnix (excluding protected).
		candidates := make([]Version, 0, max)
		for i := 0; i < max; i++ {
			v, ok := used[i]
			if !ok {
				continue
			}
			if protectedNums != nil && protectedNums[v.N] {
				continue
			}
			candidates = append(candidates, v)
		}
		if len(candidates) == 0 {
			return "", fmt.Errorf("queue mode: cannot overwrite any backups (all slots are protected); refusing")
		}

		sort.Slice(candidates, func(i, j int) bool {
			// Oldest first
			if candidates[i].CreatedUnix == candidates[j].CreatedUnix {
				return candidates[i].N < candidates[j].N
			}
			return candidates[i].CreatedUnix < candidates[j].CreatedUnix
		})
		slot = candidates[0].N
	}

	if slot < 0 || slot >= max {
		return "", fmt.Errorf("internal error: could not determine backup slot")
	}

	dst := filepath.Join(projectRoot, fmt.Sprintf("%s_%d", project, slot))

	// Overwrite slot dir
	_ = os.RemoveAll(dst)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return "", fmt.Errorf("create dest: %w", err)
	}
	if err := copyDirContents(srcAbs, dst); err != nil {
		_ = os.RemoveAll(dst)
		return "", err
	}
	if err := writeMetaAtomic(dst, time.Now()); err != nil {
		_ = os.RemoveAll(dst)
		return "", err
	}

	return dst, nil
}

func listProjectVersions(projectRoot, project string) ([]Version, error) {
	ents, err := os.ReadDir(projectRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read project root: %w", err)
	}

	prefix := project + "_"
	out := make([]Version, 0, len(ents))

	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		nStr := strings.TrimPrefix(name, prefix)
		n, err := strconv.Atoi(nStr)
		if err != nil {
			continue
		}

		full := filepath.Join(projectRoot, name)
		createdUnix, hasMeta, err := readCreatedUnix(full)
		if err != nil {
			return nil, err
		}

		out = append(out, Version{
			N:           n,
			Path:        full,
			CreatedUnix: createdUnix,
			HasMeta:     hasMeta,
		})
	}

	// Default sort by N (nice for list). Newest/oldest use CreatedUnix separately.
	sort.Slice(out, func(i, j int) bool { return out[i].N < out[j].N })
	return out, nil
}

// newestBackupPath returns the path to the newest backup (by meta created_unix).
// If none exist, returns "" with nil error.
func newestBackupPath(projectRoot, project string) (string, error) {
	vers, err := listProjectVersions(projectRoot, project)
	if err != nil {
		return "", err
	}
	if len(vers) == 0 {
		return "", nil
	}
	sort.Slice(vers, func(i, j int) bool {
		// Newest first; tie-breaker: higher N.
		if vers[i].CreatedUnix == vers[j].CreatedUnix {
			return vers[i].N > vers[j].N
		}
		return vers[i].CreatedUnix > vers[j].CreatedUnix
	})
	return vers[0].Path, nil
}

// cleanseBackupRoot deletes everything directly under backupRoot except cfgPath.
// It returns number removed.
func cleanseBackupRoot(backupRoot, cfgPath string) (int, error) {
	entries, err := os.ReadDir(backupRoot)
	if err != nil {
		return 0, fmt.Errorf("read backup root: %w", err)
	}

	cfgBase := filepath.Base(cfgPath)
	removed := 0

	for _, e := range entries {
		name := e.Name()
		full := filepath.Join(backupRoot, name)

		if name == cfgBase {
			continue
		}
		if err := os.RemoveAll(full); err != nil {
			return removed, fmt.Errorf("remove %s: %w", full, err)
		}
		removed++
	}

	return removed, nil
}

// -------------------- COPY + REPLACE IMPLEMENTATION --------------------

func copyDirContents(srcDir, dstDir string) error {
	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		dstPath := filepath.Join(dstDir, rel)

		info, err := d.Info()
		if err != nil {
			return err
		}

		// Handle symlinks.
		if d.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
				return err
			}
			_ = os.RemoveAll(dstPath)
			return os.Symlink(target, dstPath)
		}

		if d.IsDir() {
			if err := os.MkdirAll(dstPath, info.Mode().Perm()); err != nil {
				return err
			}
			_ = os.Chtimes(dstPath, time.Now(), info.ModTime())
			return nil
		}

		// Regular file → copy bytes.
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			return err
		}
		if err := copyFile(path, dstPath, info.Mode()); err != nil {
			return err
		}
		_ = os.Chtimes(dstPath, time.Now(), info.ModTime())
		return nil
	})
}

func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	_ = os.RemoveAll(dst)
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// replaceDirContents replaces the contents of dstDir with the contents of srcDir,
// leaving dstDir itself in place. It stages the source into a temp dir first, then
// clears dstDir, then copies staged contents into dstDir.
func replaceDirContents(dstDir, srcDir string) error {
	dstDir = mustAbs(dstDir)
	srcDir = mustAbs(srcDir)

	if fi, err := os.Stat(dstDir); err != nil || !fi.IsDir() {
		return fmt.Errorf("destination is not a directory: %s", dstDir)
	}
	if fi, err := os.Stat(srcDir); err != nil || !fi.IsDir() {
		return fmt.Errorf("source is not a directory: %s", srcDir)
	}

	parent := filepath.Dir(dstDir)
	stage, err := os.MkdirTemp(parent, ".bkup-stage-*")
	if err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	defer os.RemoveAll(stage)

	if err := copyDirContents(srcDir, stage); err != nil {
		return fmt.Errorf("stage copy: %w", err)
	}

	if err := removeDirContents(dstDir); err != nil {
		return fmt.Errorf("clear destination: %w", err)
	}

	if err := copyDirContents(stage, dstDir); err != nil {
		return fmt.Errorf("restore staged into destination: %w", err)
	}

	return nil
}

func removeDirContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		if err := os.RemoveAll(full); err != nil {
			return err
		}
	}
	return nil
}

// -------------------- SHELL + EDITOR --------------------

func openSubshell(dir string) error {
	dir = mustAbs(dir)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("not a directory: %s", dir)
	}

	shell, shellArgs := defaultShell()
	cmd := exec.Command(shell, shellArgs...)
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	fmt.Println("Entering subshell in:", dir)
	fmt.Println("(exit to return)")
	return cmd.Run()
}

func defaultShell() (string, []string) {
	if runtime.GOOS != "windows" {
		if sh := os.Getenv("SHELL"); sh != "" {
			return sh, []string{"-l"}
		}
		return "/bin/sh", []string{"-l"}
	}

	if ps := findOnPath("pwsh.exe"); ps != "" {
		return ps, []string{"-NoLogo"}
	}
	if ps := findOnPath("powershell.exe"); ps != "" {
		return ps, []string{"-NoLogo"}
	}
	return "cmd.exe", []string{}
}

func openEditor(path string) error {
	path = mustAbs(path)

	// Prefer $EDITOR on all platforms.
	if ed := strings.TrimSpace(os.Getenv("EDITOR")); ed != "" {
		parts := splitCommand(ed)
		cmd := exec.Command(parts[0], append(parts[1:], path)...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	if runtime.GOOS == "windows" {
		cmd := exec.Command("notepad.exe", path)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	cmd := exec.Command("vi", path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func splitCommand(s string) []string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return []string{"vi"}
	}
	return fields
}

func findOnPath(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}
