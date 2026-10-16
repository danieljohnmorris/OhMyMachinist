package runner

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// untrackedFiles lists the files git neither tracks nor ignores. This is the
// untracked half of the dirty-lane definition (MACH-62): the same set the
// `git status --porcelain` preflight refuses.
func untrackedFiles(repository string) ([]string, error) {
	command := exec.Command("git", "-C", repository, "ls-files", "--others", "--exclude-standard", "-z")
	command.Env = sanitizedEnvironment(os.Environ())
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("list untracked files: %w", err)
	}
	var files []string
	for _, name := range strings.Split(string(output), "\x00") {
		if name != "" {
			files = append(files, name)
		}
	}
	return files, nil
}

// archiveUntracked writes the lane's untracked files to a tar next to the run
// artifacts so they survive the stash and can be recovered later. It returns
// how many files were written.
func archiveUntracked(repository, runDirectory, label string) (int, error) {
	if runDirectory == "" {
		return 0, nil
	}
	files, err := untrackedFiles(repository)
	if err != nil || len(files) == 0 {
		return 0, err
	}
	path := filepath.Join(runDirectory, label+".untracked.tar")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("create untracked archive: %w", err)
	}
	writer := tar.NewWriter(file)
	written := 0
	for _, name := range files {
		full := filepath.Join(repository, name)
		info, err := os.Lstat(full)
		if err != nil {
			continue
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, _ = os.Readlink(full)
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			continue
		}
		header.Name = name
		if err := writer.WriteHeader(header); err != nil {
			break
		}
		if info.Mode().IsRegular() {
			source, err := os.Open(full)
			if err != nil {
				continue
			}
			_, copyErr := io.Copy(writer, source)
			_ = source.Close()
			if copyErr != nil {
				break
			}
		}
		written++
	}
	closeErr := writer.Close()
	fileErr := file.Close()
	if err := errors.Join(closeErr, fileErr); err != nil {
		return written, err
	}
	return written, nil
}
