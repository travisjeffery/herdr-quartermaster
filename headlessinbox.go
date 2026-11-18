package main

import (
	"os"
	"path/filepath"
)

// Only verified outcomes archive the enrolled event's exact inbox files. An
// explicit coordination blocker stays visible in both inbox and report outbox.
// Renaming is replay-safe; a crash cannot turn model delivery into completion.
func headlessArchive(en headlessEnrollment, e *headlessEvent) error {
	items, err := readInbox()
	if err != nil {
		return err
	}
	done := filepath.Join(inboxDir(), "done")
	if err = os.MkdirAll(done, 0700); err != nil {
		return err
	}
	for _, it := range items {
		if it.Bead != en.Bead || it.Kind != e.Kind || it.At.Before(en.Since) {
			continue
		}
		if err = os.Rename(filepath.Join(inboxDir(), it.File), filepath.Join(done, it.File)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
