//go:build !windows

package metadata

import (
	"errors"
	"fmt"
	"os"
)

func createPrivateSpool() (*os.File, error) {
	file, err := os.CreateTemp("", "ptah-ydb-metadata-*")
	if err != nil {
		return nil, fmt.Errorf("create YDB metadata snapshot failed")
	}
	if err := os.Remove(file.Name()); err != nil {
		closeErr := file.Close()
		removeErr := os.Remove(file.Name())
		return nil, errors.Join(fmt.Errorf("unlink YDB metadata snapshot failed"), closeErr, removeErr)
	}
	return file, nil
}
