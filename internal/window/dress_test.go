package window

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestThemeAndControlsStayChecked(t *testing.T) {
	st := NewStage(StageOptions{})
	defer st.Close()
	reg := New(Options{})
	reg.Register(st)

	bad := DefaultTheme()
	bad.Accent = "red; background:url(http://x)"
	if err := st.SetTheme(bad); err == nil {
		t.Fatal("unsafe color must be refused")
	}
	if st.Theme().Accent != DefaultTheme().Accent {
		t.Fatal("refused theme was applied")
	}

	next := DefaultTheme()
	next.Accent = "#88aaff"
	next.Font = FontSerif
	next.Radius = RadiusRound
	next.Frame = FrameGlow
	if err := st.SetTheme(next); err != nil {
		t.Fatal(err)
	}
	if err := st.AddControls([]Control{{Kind: "script", Text: "<script>"}}); err == nil {
		t.Fatal("unknown control must be refused")
	}
	if err := st.AddControls([]Control{
		{ID: "title", Kind: ControlLabel, Text: "春日"},
		{ID: "warm", Kind: ControlMeter, Text: "warmth", Value: 0.4},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Apply("stage", OpOpen, ""); err != nil {
		t.Fatal(err)
	}
	glance := reg.Glance()
	for _, want := range []string{"#88aaff", "serif", "round", "glowing", "label", "春日", "meter"} {
		if !strings.Contains(glance, want) {
			t.Fatalf("glance %q missing %q", glance, want)
		}
	}

	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/desktop", nil))
	var body struct {
		View struct {
			Theme    Theme     `json:"theme"`
			Controls []Control `json:"controls"`
		} `json:"view"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.View.Theme.Accent != "#88aaff" || len(body.View.Controls) != 2 {
		t.Fatalf("view %+v", body.View)
	}
	if _, err := reg.Apply("stage", OpClearControls, ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(reg.Glance(), "春日") {
		t.Fatal("cleared control still in the glance")
	}
}

func TestParseDraftsFromModelText(t *testing.T) {
	theme, err := ParseTheme("sure {\"background\":\"#fff8f0\",\"desk\":\"#fff1e6\",\"ink\":\"#2b2118\",\"muted\":\"#8a7568\",\"accent\":\"#c46b4a\",\"card\":\"#fffaf6\",\"line\":\"#ecd9cc\",\"font\":\"serif\",\"radius\":\"soft\",\"frame\":\"line\"}")
	if err != nil {
		t.Fatal(err)
	}
	if theme.Font != FontSerif || theme.Accent != "#c46b4a" {
		t.Fatalf("%+v", theme)
	}
	list, err := ParseControls("{\"controls\":[{\"kind\":\"chip\",\"text\":\"today\"},{\"kind\":\"rule\"}]}")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Kind != ControlChip || list[1].Kind != ControlRule {
		t.Fatalf("%+v", list)
	}
	if _, err := ParseControls("{\"controls\":[{\"kind\":\"html\",\"text\":\"<b>x</b>\"}]}"); err == nil {
		t.Fatal("html kind must be refused")
	}
}
