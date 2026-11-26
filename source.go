package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/knoniewicz/kor/messages"
)

// maxSourceBytes caps what kron.source returns. Jobs are small Lua files; the cap
// keeps a mistakenly huge file from being shipped through Kor.
const maxSourceBytes = 256 << 10

type sourceOut struct {
	Name string `json:"name"`
	File string `json:"file"`
	Lua  string `json:"lua"`
}

// source is the read-only handler: it resolves a loaded job by name and returns the
// Lua file Kron loaded for it. The payload is only a name, never a path.
func (k *Kron) source(_ context.Context, m messages.Message) (any, error) {
	var in nameIn
	if err := m.DecodePayload(&in); err != nil {
		return nil, messages.Errorf(messages.ErrorCodeInvalid, "source payload: %v", err)
	}
	return k.readSource(in.Name)
}

func (k *Kron) readSource(name string) (sourceOut, error) {
	j := k.job(name)
	if j == nil {
		return sourceOut{}, messages.Errorf(messages.ErrorCodeNotFound, "no job %q", name)
	}
	path, err := k.jobFile(j)
	if err != nil {
		return sourceOut{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return sourceOut{}, messages.Errorf(messages.ErrorCodeNotFound, "job %q source unavailable: %v", name, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return sourceOut{}, messages.Errorf(messages.ErrorCodeInternal, "job %q source: %v", name, err)
	}
	if !st.Mode().IsRegular() {
		return sourceOut{}, messages.Errorf(messages.ErrorCodeInvalid, "job %q source is not a regular file", name)
	}
	if st.Size() > maxSourceBytes {
		return sourceOut{}, sourceTooBig(name, st.Size())
	}
	b, err := io.ReadAll(io.LimitReader(f, maxSourceBytes+1))
	if err != nil {
		return sourceOut{}, messages.Errorf(messages.ErrorCodeInternal, "job %q source: %v", name, err)
	}
	if len(b) > maxSourceBytes {
		// The file grew between Stat and Read.
		return sourceOut{}, sourceTooBig(name, int64(len(b)))
	}
	return sourceOut{Name: j.Name, File: filepath.Clean(j.File), Lua: string(b)}, nil
}

func sourceTooBig(name string, size int64) error {
	return messages.Errorf(messages.ErrorCodeInvalid, "job %q source is %d bytes, limit is %d", name, size, maxSourceBytes)
}

// jobFile only trusts the path Kron itself recorded while loading the job, and checks
// that it still sits under the jobs directory. A caller cannot pass an arbitrary path.
func (k *Kron) jobFile(j *Job) (string, error) {
	if k.cfg.Dir == "" {
		return "", messages.Errorf(messages.ErrorCodeInvalid, "job %q: no job directory configured", j.Name)
	}
	absDir, err := filepath.Abs(k.cfg.Dir)
	if err != nil {
		return "", messages.Errorf(messages.ErrorCodeInvalid, "job %q: %v", j.Name, err)
	}
	absFile, err := filepath.Abs(j.File)
	if err != nil {
		return "", messages.Errorf(messages.ErrorCodeInvalid, "job %q: %v", j.Name, err)
	}
	rel, err := filepath.Rel(absDir, absFile)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", messages.Errorf(messages.ErrorCodeInvalid, "job %q: file is outside %s", j.Name, k.cfg.Dir)
	}
	if filepath.Ext(absFile) != ".lua" {
		return "", messages.Errorf(messages.ErrorCodeInvalid, "job %q: file is not Lua", j.Name)
	}
	return absFile, nil
}
