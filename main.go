// bkup: simple cross-platform directory backup + "go/revert/clean" helpers.
//
// Usage:
//   bkup                   # backup current dir -> $HOME/.bkup/<name>_backup (overwrite)
//   bkup go [--print]      # backup, save prev.txt, then open a subshell in backup dir (or print dir)
//   bkup revert [--print]  # open a subshell in dir stored in prev.txt (or print dir)
//   bkup clean             # delete all backups under $HOME/.bkup, keep prev.txt if present
//
// Backup root:
//   $HOME/.bkup   (works on macOS/Linux/Windows via os.UserHomeDir)
//
// Build:
//   go build -o bkup .
//   (put bkup on your PATH)

package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	backupFolderName = ".bkup"
	prevFileName     = "prev.txt"
)

func main() {
	args := os.Args[1:]

	printMode := false
	filtered := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--print" {
			printMode = true
			continue
		}
		filtered = append(filtered, a)
	}
	args = filtered

	backupRoot, err := getBackupRoot()
	if err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(backupRoot, 0o755); err != nil {
		fatal(fmt.Errorf("create backup root: %w", err))
	}

	switch {
	case len(args) == 0:
		// bkup
		cwd, err := os.Getwd()
		if err != nil {
			fatal(err)
		}
		dst, err := backupCurrentDir(cwd, backupRoot)
		if err != nil {
			fatal(err)
		}
		fmt.Println(dst)

	case len(args) >= 1 && args[0] == "go":
		// bkup go [--print]
		cwd, err := os.Getwd()
		if err != nil {
			fatal(err)
		}
		dst, err := backupCurrentDir(cwd, backupRoot)
		if err != nil {
			fatal(err)
		}
		if err := writePrev(backupRoot, cwd); err != nil {
			fatal(err)
		}

		if printMode {
			fmt.Println(dst)
			return
		}
		if err := openSubshell(dst); err != nil {
			fatal(err)
		}

	case len(args) >= 1 && args[0] == "revert":
		// bkup revert [--print]
		prev, err := readPrev(backupRoot)
		if err != nil {
			fatal(err)
		}

		if printMode {
			fmt.Println(prev)
			return
		}
		if err := openSubshell(prev); err != nil {
			fatal(err)
		}

	case len(args) >= 1 && args[0] == "clean":
		// bkup clean
		removed, keptPrev, err := cleanBackupRoot(backupRoot)
		if err != nil {
			fatal(err)
		}
		if keptPrev {
			fmt.Printf("Cleaned %d item(s). Kept %s.\n", removed, filepath.Join(backupRoot, prevFileName))
		} else {
			fmt.Printf("Cleaned %d item(s). (No %s present to keep.)\n", removed, prevFileName)
		}

	case len(args) >= 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help"):
		usage()

	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println(`bkup - backup current directory into a shared cross-platform backup location

Usage:
  bkup
      Back up current directory to: $HOME/.bkup/<dirname>_backup (overwrites)

  bkup go [--print]
      Back up current directory, write $HOME/.bkup/prev.txt with current path,
      then open a subshell in the backup directory.
      With --print: just print the backup directory path.

  bkup revert [--print]
      Read $HOME/.bkup/prev.txt and open a subshell in that directory.
      With --print: just print the previous directory path.

  bkup clean
      Delete all backups under $HOME/.bkup, keeping prev.txt if it exists.
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

func backupCurrentDir(srcAbs string, backupRoot string) (string, error) {
	srcAbs, err := filepath.Abs(srcAbs)
	if err != nil {
		return "", err
	}
	base := filepath.Base(srcAbs)
	dst := filepath.Join(backupRoot, base+"_backup")

	// Overwrite any existing backup.
	_ = os.RemoveAll(dst)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return "", fmt.Errorf("create dest: %w", err)
	}

	// Copy the directory contents into dst (not nesting the dir again).
	if err := copyDirContents(srcAbs, dst); err != nil {
		_ = os.RemoveAll(dst)
		return "", err
	}

	return dst, nil
}

// cleanBackupRoot deletes everything directly under backupRoot except prev.txt.
// It returns: number removed, whether prev.txt existed (kept), error.
func cleanBackupRoot(backupRoot string) (int, bool, error) {
	entries, err := os.ReadDir(backupRoot)
	if err != nil {
		return 0, false, fmt.Errorf("read backup root: %w", err)
	}

	removed := 0
	keptPrev := false

	for _, e := range entries {
		name := e.Name()
		full := filepath.Join(backupRoot, name)

		// Keep prev.txt if present.
		if name == prevFileName && !e.IsDir() {
			keptPrev = true
			continue
		}

		// Remove everything else (dirs or files).
		if err := os.RemoveAll(full); err != nil {
			return removed, keptPrev, fmt.Errorf("remove %s: %w", full, err)
		}
		removed++
	}

	return removed, keptPrev, nil
}

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

func writePrev(backupRoot, prevPath string) error {
	prevPath, err := filepath.Abs(prevPath)
	if err != nil {
		return err
	}
	p := filepath.Join(backupRoot, prevFileName)
	return os.WriteFile(p, []byte(prevPath+"\n"), 0o644)
}

func readPrev(backupRoot string) (string, error) {
	p := filepath.Join(backupRoot, prevFileName)
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("read %s: %w (run `bkup go` first)", p, err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("%s is empty (run `bkup go` first)", p)
	}
	return s, nil
}

func openSubshell(dir string) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
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

func findOnPath(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}
