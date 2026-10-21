//go:build windows

package metadata

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func createPrivateSpool() (*os.File, error) {
	name := filepath.Join(os.TempDir(), "ptah-ydb-metadata-"+rand.Text())
	pointer, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("invalid YDB metadata snapshot path")
	}
	// DELETE access and DELETE_ON_CLOSE make cleanup an operating-system handle
	// property, including when the provider exits without running Go defers.
	handle, err := windows.CreateFile(pointer, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE, 0, nil, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_TEMPORARY|windows.FILE_FLAG_DELETE_ON_CLOSE, 0)
	if err != nil {
		return nil, fmt.Errorf("create YDB metadata snapshot failed")
	}
	file := os.NewFile(uintptr(handle), name)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("open YDB metadata snapshot handle failed")
	}
	return file, nil
}
