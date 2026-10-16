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
	var failures []error
	for _, name := range files {
		full := filepath.Join(repository, name)
		info, err := os.Lstat(full)
		if err != nil {
			failures = append(failures, fmt.Errorf("archive %s: %w", name, err))
			continue
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(full); err != nil {
				failures = append(failures, fmt.Errorf("archive %s: %w", name, err))
				continue
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			failures = append(failures, fmt.Errorf("archive %s: %w", name, err))
			continue
		}
		header.Name = name
		// Open the file before writing its header: a header already written
		// for a body we fail to open would leave the stream unreadable, so a
		// failure here must not be followed by more entries.
		var source *os.File
		if info.Mode().IsRegular() {
			if source, err = os.Open(full); err != nil {
				failures = append(failures, fmt.Errorf("archive %s: %w", name, err))
				continue
			}
		}
		if err := writer.WriteHeader(header); err != nil {
			if source != nil {
				_ = source.Close()
			}
			failures = append(failures, fmt.Errorf("archive %s: %w", name, err))
			break
		}
		if source != nil {
			// A copy error (short read from the source or a failed write to
			// the archive) leaves a truncated entry that tar.Writer.Close may
			// not re-report, so it is recorded here.
			_, copyErr := io.Copy(writer, source)
			if copyErr = errors.Join(copyErr, source.Close()); copyErr != nil {
				failures = append(failures, fmt.Errorf("archive %s: %w", name, copyErr))
				break
			}
		}
		written++
	}
	if err := writer.Close(); err != nil {
		failures = append(failures, fmt.Errorf("close untracked archive: %w", err))
	}
	if err := file.Close(); err != nil {
		failures = append(failures, fmt.Errorf("close untracked archive file: %w", err))
	}
	return written, errors.Join(failures...)
}
