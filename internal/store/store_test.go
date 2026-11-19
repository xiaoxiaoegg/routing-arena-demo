// internal/store/store_test.go
// JSONL 存储测试：追加顺序、读取恢复、坏行容错、文件缺失。

package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type item struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

func TestAppendAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "history.jsonl")

	// 追加 3 条
	for i := 1; i <= 3; i++ {
		if err := Append(p, item{ID: i, Name: "n" + string(rune('a'+i-1))}); err != nil {
			t.Fatalf("Append 失败: %v", err)
		}
	}

	// 读回，顺序必须一致
	var got []item
	found, n, skipped, err := Load(p, func(b []byte) error {
		var it item
		if err := json.Unmarshal(b, &it); err != nil {
			return err
		}
		got = append(got, it)
		return nil
	})
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if !found || n != 3 || skipped != 0 {
		t.Fatalf("found=%v n=%d skipped=%d", found, n, skipped)
	}
	for i, it := range got {
		if it.ID != i+1 || it.Name != "n"+string(rune('a'+i)) {
			t.Errorf("第 %d 条不一致: %+v", i, it)
		}
	}
}

func TestAppendAppendsNotOverwrites(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.jsonl")
	_ = Append(p, item{ID: 1, Name: "a"})
	_ = Append(p, item{ID: 2, Name: "b"})

	n, _ := os.ReadFile(p)
	lines := 0
	for _, b := range n {
		if b == '\n' {
			lines++
		}
	}
	if lines != 2 {
		t.Fatalf("应为 2 行（追加不覆盖），实际 %d 行", lines)
	}
}

func TestLoadMissingFile(t *testing.T) {
	found, n, skipped, err := Load(filepath.Join(t.TempDir(), "nope.jsonl"), func([]byte) error { return nil })
	if err != nil || found || n != 0 || skipped != 0 {
		t.Fatalf("文件缺失应 (false,0,0,nil)，实际 (%v,%d,%d,%v)", found, n, skipped, err)
	}
}

func TestLoadSkipsBadLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.jsonl")
	os.WriteFile(p, []byte("{\"id\":1}\nnot-json\n{\"id\":2}\n"), 0o644)

	var ids []int
	_, n, skipped, err := Load(p, func(b []byte) error {
		var it item
		if err := json.Unmarshal(b, &it); err != nil {
			return err
		}
		ids = append(ids, it.ID)
		return nil
	})
	if err != nil {
		t.Fatalf("坏行不应导致整体失败: %v", err)
	}
	if n != 2 || skipped != 1 {
		t.Fatalf("应处理 2 行、跳过 1 行，实际 n=%d skipped=%d", n, skipped)
	}
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Errorf("好行应完整读回: %v", ids)
	}
}
