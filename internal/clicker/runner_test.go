package clicker

import (
	"context"
	"errors"
	"testing"

	"pointrunner/internal/config"
)

func TestSafeClickContainsPanics(t *testing.T) {
	err := safeClick(context.Background(), func(context.Context, config.Point) error {
		panic("test panic")
	}, config.Point{})
	if err == nil || err.Error() != "click callback panicked: test panic" {
		t.Fatalf("expected recovered click panic, got %v", err)
	}
}

func TestSafeClickPropagatesErrors(t *testing.T) {
	want := errors.New("click failed")
	err := safeClick(context.Background(), func(context.Context, config.Point) error { return want }, config.Point{})
	if !errors.Is(err, want) {
		t.Fatalf("expected original error, got %v", err)
	}
}
