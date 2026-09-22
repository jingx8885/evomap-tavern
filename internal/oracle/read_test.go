package oracle

import (
	"context"
	"strings"
	"testing"
)

type fakeLLM struct {
	out string
	err error
	saw string
}

func (f *fakeLLM) ChatComplete(ctx context.Context, system, user string) (string, error) {
	f.saw = system + "\n" + user
	return f.out, f.err
}

func TestReadKeepsOmenClosed(t *testing.T) {
	f := &fakeLLM{out: "好的 {\"note\":\"让她说这卦宜守\",\"omen\":\"大吉\",\"focus\":\"妻财五爻\"}"}
	got, err := Read(context.Background(), f, "小春", "短", "盘", "这事成不成")
	if err != nil {
		t.Fatal(err)
	}
	if got.Omen != "未明" || got.Note != "让她说这卦宜守" || got.Focus != "妻财五爻" {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(f.saw, "不许改爻") || !strings.Contains(f.saw, "这事成不成") {
		t.Fatalf("prompt missing plate rules: %s", f.saw)
	}
}

func TestReadRefuse(t *testing.T) {
	f := &fakeLLM{out: `{"note":"陪着她","omen":"refuse","focus":""}`}
	got, err := Read(context.Background(), f, "", "", "盘", "不想活了")
	if err != nil {
		t.Fatal(err)
	}
	if got.Omen != "refuse" {
		t.Fatal(got.Omen)
	}
}
