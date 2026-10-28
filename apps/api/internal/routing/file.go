package routing

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

const maxConfigBytes = 16 << 10

func (m *Manager) read(projectID string) (Binding, error) {
	name := routeName(projectID)
	info, err := m.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return Binding{}, ErrNotFound
	}
	if err != nil {
		return Binding{}, fmt.Errorf("inspect route file: %w", err)
	}
	if !privateOwned(info, false) || info.Size() > maxConfigBytes {
		return Binding{}, ErrNotOwned
	}
	file, err := m.root.Open(name)
	if err != nil {
		return Binding{}, fmt.Errorf("open route file: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return Binding{}, ErrNotOwned
	}
	data, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil {
		return Binding{}, fmt.Errorf("read route file: %w", err)
	}
	header, _, found := bytes.Cut(data, []byte("\n"))
	if !found || len(data) > maxConfigBytes || !bytes.HasPrefix(header, []byte(headerPrefix)) {
		return Binding{}, ErrNotOwned
	}
	var binding Binding
	if err := json.Unmarshal(header[len(headerPrefix):], &binding); err != nil || !validBinding(binding) ||
		binding.ProjectID != projectID || !bytes.Equal(data, render(binding)) {
		return Binding{}, ErrNotOwned
	}
	return binding, nil
}

func (m *Manager) write(ctx context.Context, binding Binding) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("allocate route temporary file: %w", err)
	}
	temporary := ".shipyard-" + hex.EncodeToString(nonce[:]) + ".tmp"
	file, err := m.root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create route temporary file: %w", err)
	}
	defer m.root.Remove(temporary)
	data := render(binding)
	count, writeErr := file.Write(data)
	if writeErr == nil && count != len(data) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("write route temporary file: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.root.Rename(temporary, routeName(binding.ProjectID)); err != nil {
		return fmt.Errorf("publish route file: %w", err)
	}
	return m.syncDirectory()
}

func (m *Manager) syncDirectory() error {
	directory, err := m.root.Open(".")
	if err != nil {
		return fmt.Errorf("open route directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync route directory after mutation: %w", err)
	}
	return nil
}
