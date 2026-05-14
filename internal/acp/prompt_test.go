package acp

import (
	"encoding/json"
	"testing"

	"github.com/tiru-r/pi-agent-go/internal/model"
)

func TestPromptContent_LegacyString(t *testing.T) {
	var p promptContent
	if err := json.Unmarshal([]byte(`"hello world"`), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Blocks) != 1 {
		t.Fatalf("want 1 block, got %d", len(p.Blocks))
	}
	if p.Blocks[0].Type != model.ContentTypeText || p.Blocks[0].Text != "hello world" {
		t.Errorf("unexpected block: %+v", p.Blocks[0])
	}
}

func TestPromptContent_EmptyString(t *testing.T) {
	var p promptContent
	if err := json.Unmarshal([]byte(`""`), &p); err != nil {
		t.Fatal(err)
	}
	if !p.IsEmpty() {
		t.Error("expected empty for blank string")
	}
}

func TestPromptContent_Null(t *testing.T) {
	var p promptContent
	if err := json.Unmarshal([]byte(`null`), &p); err != nil {
		t.Fatal(err)
	}
	if !p.IsEmpty() {
		t.Error("expected empty for null")
	}
}

func TestPromptContent_TextBlock(t *testing.T) {
	raw := `[{"type":"text","text":"ask me anything"}]`
	var p promptContent
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Blocks) != 1 || p.Blocks[0].Type != model.ContentTypeText {
		t.Fatalf("unexpected blocks: %+v", p.Blocks)
	}
	if p.Blocks[0].Text != "ask me anything" {
		t.Errorf("unexpected text: %q", p.Blocks[0].Text)
	}
}

func TestPromptContent_ImageDataURL(t *testing.T) {
	raw := `[{"type":"image","image":{"url":"data:image/png;base64,abc123","alt":"screenshot"}}]`
	var p promptContent
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Blocks) != 1 || p.Blocks[0].Type != model.ContentTypeImage {
		t.Fatalf("unexpected blocks: %+v", p.Blocks)
	}
	src := p.Blocks[0].Source
	if src == nil {
		t.Fatal("Source is nil")
	}
	if src.Type != "base64" || src.MediaType != "image/png" || src.Data != "abc123" {
		t.Errorf("unexpected source: %+v", src)
	}
}

func TestPromptContent_ImageURL(t *testing.T) {
	raw := `[{"type":"image","image":{"url":"https://example.com/img.jpg"}}]`
	var p promptContent
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Blocks) != 1 || p.Blocks[0].Type != model.ContentTypeImage {
		t.Fatalf("unexpected blocks: %+v", p.Blocks)
	}
	src := p.Blocks[0].Source
	if src.Type != "url" || src.URL != "https://example.com/img.jpg" {
		t.Errorf("unexpected source: %+v", src)
	}
}

func TestPromptContent_ImageSourceShape(t *testing.T) {
	raw := `[{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"xyz"}}]`
	var p promptContent
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Blocks) != 1 || p.Blocks[0].Type != model.ContentTypeImage {
		t.Fatalf("unexpected blocks: %+v", p.Blocks)
	}
	src := p.Blocks[0].Source
	if src.Type != "base64" || src.MediaType != "image/jpeg" || src.Data != "xyz" {
		t.Errorf("unexpected source: %+v", src)
	}
}

func TestPromptContent_ImageMissingSource_Dropped(t *testing.T) {
	raw := `[{"type":"image"},{"type":"text","text":"after"}]`
	var p promptContent
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	// malformed image block dropped; text block survives
	if len(p.Blocks) != 1 || p.Blocks[0].Type != model.ContentTypeText {
		t.Errorf("unexpected blocks: %+v", p.Blocks)
	}
}

func TestPromptContent_FileBlock(t *testing.T) {
	raw := `[{"type":"file","path":"src/main.go","text":"package main"}]`
	var p promptContent
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Blocks) != 1 || p.Blocks[0].Type != model.ContentTypeText {
		t.Fatalf("unexpected blocks: %+v", p.Blocks)
	}
	text := p.Blocks[0].Text
	if text != "**File: src/main.go**\n```\npackage main\n```" {
		t.Errorf("unexpected text: %q", text)
	}
}

func TestPromptContent_SelectionBlock(t *testing.T) {
	raw := `[{"type":"selection","path":"main.go","text":"x := 1","start":{"line":4},"end":{"line":4}}]`
	var p promptContent
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Blocks) != 1 || p.Blocks[0].Type != model.ContentTypeText {
		t.Fatalf("unexpected blocks: %+v", p.Blocks)
	}
	text := p.Blocks[0].Text
	if text != "**Selection from main.go (line 5):**\n```\nx := 1\n```" {
		t.Errorf("unexpected text: %q", text)
	}
}

func TestPromptContent_SelectionMultiLine(t *testing.T) {
	raw := `[{"type":"selection","path":"a.go","text":"code","start":{"line":2},"end":{"line":5}}]`
	var p promptContent
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	text := p.Blocks[0].Text
	if text != "**Selection from a.go (line 3–6):**\n```\ncode\n```" {
		t.Errorf("unexpected text: %q", text)
	}
}

func TestPromptContent_SymbolBlock(t *testing.T) {
	raw := `[{"type":"symbol","name":"Agent","path":"agent.go","text":"type Agent struct{}"}]`
	var p promptContent
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	text := p.Blocks[0].Text
	if text != "**Symbol: Agent in agent.go:**\n```\ntype Agent struct{}\n```" {
		t.Errorf("unexpected text: %q", text)
	}
}

func TestPromptContent_BranchDiff(t *testing.T) {
	raw := `[{"type":"branch_diff","text":"+added\n-removed"}]`
	var p promptContent
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	text := p.Blocks[0].Text
	if text != "**Branch Diff:**\n```diff\n+added\n-removed\n```" {
		t.Errorf("unexpected text: %q", text)
	}
}

func TestPromptContent_Thread(t *testing.T) {
	raw := `[{"type":"thread","text":"prior discussion"}]`
	var p promptContent
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	text := p.Blocks[0].Text
	if text != "**Thread context:**\nprior discussion" {
		t.Errorf("unexpected text: %q", text)
	}
}

func TestPromptContent_Rules(t *testing.T) {
	raw := `[{"type":"rules","text":"always use tabs"}]`
	var p promptContent
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	text := p.Blocks[0].Text
	if text != "**Rules:**\nalways use tabs" {
		t.Errorf("unexpected text: %q", text)
	}
}

func TestPromptContent_UnknownType_PreservesText(t *testing.T) {
	raw := `[{"type":"future_type","text":"some content"}]`
	var p promptContent
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Blocks) != 1 || p.Blocks[0].Text != "some content" {
		t.Errorf("unexpected blocks: %+v", p.Blocks)
	}
}

func TestPromptContent_Mixed(t *testing.T) {
	raw := `[
		{"type":"text","text":"look at this"},
		{"type":"image","image":{"url":"data:image/png;base64,iVBOR"}},
		{"type":"file","path":"foo.go","text":"package foo"}
	]`
	var p promptContent
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Blocks) != 3 {
		t.Fatalf("want 3 blocks, got %d: %+v", len(p.Blocks), p.Blocks)
	}
	if p.Blocks[0].Type != model.ContentTypeText {
		t.Errorf("block 0: want text, got %s", p.Blocks[0].Type)
	}
	if p.Blocks[1].Type != model.ContentTypeImage {
		t.Errorf("block 1: want image, got %s", p.Blocks[1].Type)
	}
	if p.Blocks[2].Type != model.ContentTypeText {
		t.Errorf("block 2: want text (formatted file), got %s", p.Blocks[2].Type)
	}
}
