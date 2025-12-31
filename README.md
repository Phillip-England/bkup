# bkup

A small, cross-platform Go CLI for backing up your current working directory and quickly jumping between the original location and its backup.

`bkup` is designed to be:
- **Simple** – one binary, no config required
- **Cross-platform** – works on macOS, Linux, and Windows
- **Safe by default** – backups live in a single, predictable location under your home directory

---

## Features

- 📦 **One-command backup** of the current directory and all subfiles
- ♻️ **Overwrite-safe** – existing backups with the same name are replaced
- 🚀 **Quick navigation** into the backup via a subshell
- 🔙 **Revert support** – jump back to where you started
- 🧭 **Shell-friendly mode** for true `cd` integration

---

## How It Works

- All backups are stored in:

```
$HOME/.bkup/
```

- When you back up a directory named `vii`, it is copied to:

```
$HOME/.bkup/vii_backup/
```

- When using `bkup go`, your original directory is saved to:

```
$HOME/.bkup/prev.txt
```

This allows `bkup revert` to take you back later.

---

## Installation

### Build from source

```bash
go build -o bkup .
```

Move it somewhere on your `PATH`:

```bash
# macOS / Linux
sudo mv bkup /usr/local/bin/bkup
```

On Windows, place `bkup.exe` somewhere in your `%PATH%`.

---

## Usage

### Back up the current directory

```bash
bkup
```

Example:

```bash
cd /src/vii
bkup
# → creates $HOME/.bkup/vii_backup
```

---

### Back up and enter the backup directory

```bash
bkup go
```

This will:
1. Back up the current directory
2. Save the original location to `prev.txt`
3. Open a **subshell** inside the backup directory

Exit the shell to return to where you ran the command.

---

### Revert back to the previous directory

```bash
bkup revert
```

This opens a subshell in the directory saved by the last `bkup go`.

> ⚠️ You must run `bkup go` at least once before using `bkup revert`.

---

## Shell Integration (Recommended)

Because a program cannot permanently change your current shell’s working directory, `bkup` provides a `--print` mode so you can wrap it with shell functions.

### Bash / Zsh

Add this to your `~/.bashrc` or `~/.zshrc`:

```bash
bkupgo() {
  cd "$(bkup go --print)"
}

bkuprv() {
  cd "$(bkup revert --print)"
}
```

Now you get **true `cd` behavior**:

```bash
bkupgo   # cd into backup directory
bkuprv   # cd back to original directory
```

---

## Notes & Behavior

- Backups **overwrite** existing directories with the same name
- File permissions and modification times are preserved best-effort
- Symlinks are preserved as symlinks
- No compression is used (this is a straight file copy)

---

## Backup Layout Example

```
~/.bkup/
├── vii_backup/
│   ├── main.go
│   ├── go.mod
│   └── ...
└── prev.txt
```

---

## License

MIT License © 2025

---

## Why `bkup`?

`bkup` is ideal for:
- Quick safety snapshots before refactors
- Experimenting without Git commits
- Jumping into backup copies instantly
- A lightweight alternative to full backup tools

If you want extensions like compression, timestamps, or multiple backup histories, this tool is intentionally small and easy to modify.

Happy hacking 🚀

