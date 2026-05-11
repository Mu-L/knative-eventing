/*
Copyright 2024 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package utils

import (
	"sync"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
)

// logCapture is a minimal zapcore.Core that records log entries for assertions.
type logCapture struct {
	mu      sync.Mutex
	entries []zapcore.Entry
}

func (c *logCapture) Enabled(zapcore.Level) bool { return true }

func (c *logCapture) With([]zapcore.Field) zapcore.Core { return c }

func (c *logCapture) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	return ce.AddCore(e, c)
}

func (c *logCapture) Write(e zapcore.Entry, _ []zapcore.Field) error {
	c.mu.Lock()
	c.entries = append(c.entries, e)
	c.mu.Unlock()
	return nil
}

func (c *logCapture) Sync() error { return nil }

func (c *logCapture) countLevel(l zapcore.Level) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.entries {
		if e.Level == l {
			n++
		}
	}
	return n
}

func newCaptureLogger() (*zap.SugaredLogger, *logCapture) {
	cap := &logCapture{}
	return zap.New(cap).Sugar(), cap
}

func TestSetKlogVerbosityFromConfigMap(t *testing.T) {
	tests := []struct {
		name        string
		data        map[string]string
		wantApplied bool
		wantErr     bool
	}{
		{
			name:        "key absent",
			data:        map[string]string{},
			wantApplied: false,
			wantErr:     false,
		},
		{
			name:        "zero value",
			data:        map[string]string{KlogVerbosityKey: "0"},
			wantApplied: true,
			wantErr:     false,
		},
		{
			name:        "empty value",
			data:        map[string]string{KlogVerbosityKey: ""},
			wantApplied: false,
			wantErr:     false,
		},
		{
			name:        "valid level 5",
			data:        map[string]string{KlogVerbosityKey: "5"},
			wantApplied: true,
			wantErr:     false,
		},
		{
			name:        "valid level 9",
			data:        map[string]string{KlogVerbosityKey: "9"},
			wantApplied: true,
			wantErr:     false,
		},
		{
			name:        "invalid non-integer",
			data:        map[string]string{KlogVerbosityKey: "high"},
			wantApplied: false,
			wantErr:     true,
		},
		{
			name:        "invalid float",
			data:        map[string]string{KlogVerbosityKey: "3.5"},
			wantApplied: false,
			wantErr:     true,
		},
		{
			name:        "out of range level 10",
			data:        map[string]string{KlogVerbosityKey: "10"},
			wantApplied: false,
			wantErr:     true,
		},
		{
			name:        "negative level",
			data:        map[string]string{KlogVerbosityKey: "-1"},
			wantApplied: false,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			applied, err := SetKlogVerbosityFromConfigMap(tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("SetKlogVerbosityFromConfigMap() error = %v, wantErr %v", err, tt.wantErr)
			}
			if applied != tt.wantApplied {
				t.Errorf("SetKlogVerbosityFromConfigMap() applied = %v, wantApplied %v", applied, tt.wantApplied)
			}
		})
	}
}

func TestUpdateKlogVerbosityFromConfigMap(t *testing.T) {
	tests := []struct {
		name     string
		data     map[string]string
		wantWarn bool
		wantInfo bool
	}{
		{
			name:     "no key - no log",
			data:     map[string]string{},
			wantWarn: false,
			wantInfo: false,
		},
		{
			name:     "zero value - info logged",
			data:     map[string]string{KlogVerbosityKey: "0"},
			wantWarn: false,
			wantInfo: true,
		},
		{
			name:     "empty value - no log",
			data:     map[string]string{KlogVerbosityKey: ""},
			wantWarn: false,
			wantInfo: false,
		},
		{
			name:     "valid level 5 - info logged",
			data:     map[string]string{KlogVerbosityKey: "5"},
			wantWarn: false,
			wantInfo: true,
		},
		{
			name:     "invalid value - warn logged",
			data:     map[string]string{KlogVerbosityKey: "high"},
			wantWarn: true,
			wantInfo: false,
		},
		{
			name:     "out of range - warn logged",
			data:     map[string]string{KlogVerbosityKey: "10"},
			wantWarn: true,
			wantInfo: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, cap := newCaptureLogger()
			cm := &corev1.ConfigMap{Data: tt.data}
			UpdateKlogVerbosityFromConfigMap(logger)(cm)

			warnCount := cap.countLevel(zapcore.WarnLevel)
			infoCount := cap.countLevel(zapcore.InfoLevel)

			if tt.wantWarn && warnCount == 0 {
				t.Error("expected warn log, got none")
			}
			if !tt.wantWarn && warnCount > 0 {
				t.Errorf("unexpected warn log")
			}
			if tt.wantInfo && infoCount == 0 {
				t.Error("expected info log, got none")
			}
			if !tt.wantInfo && infoCount > 0 {
				t.Errorf("unexpected info log")
			}
		})
	}
}
