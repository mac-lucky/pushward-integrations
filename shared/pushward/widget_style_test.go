package pushward

import (
	"encoding/json"
	"strings"
	"testing"
)

// Nothing on the wire means the user's in-app pick applies, so unset style and
// density fields must leave their keys out of both the create and the
// merge-patch body.
func TestWidgetLooks_Wire(t *testing.T) {
	looks := WidgetContent{
		Template:      WidgetTemplateStatList,
		Style:         WidgetStyleClassic,
		StyleBySize:   map[string]WidgetStyle{"lock_screen": WidgetStyleBold},
		Density:       WidgetDensityComfortable,
		DensityBySize: map[string]string{"small": WidgetDensityCompact},
	}
	all := []string{
		`"style":"classic"`, `"style_by_size":{"lock_screen":"bold"}`,
		`"density":"comfortable"`, `"density_by_size":{"small":"compact"}`,
	}
	cases := []struct {
		name string
		body any
		want []string // nil = no style or density key at all
	}{
		{"create unset", CreateWidgetRequest{Slug: "s", Name: "S", Content: WidgetContent{Template: WidgetTemplateValue}}, nil},
		{"patch unset", UpdateWidgetRequest{Content: &WidgetContent{Value: Float64Ptr(1)}}, nil},
		{"create empty maps", CreateWidgetRequest{Slug: "s", Name: "S", Content: WidgetContent{StyleBySize: map[string]WidgetStyle{}, DensityBySize: map[string]string{}}}, nil},
		{"create set", CreateWidgetRequest{Slug: "s", Name: "S", Content: looks}, all},
		{"patch set", UpdateWidgetRequest{Content: &looks}, all},
		{"patch style only", UpdateWidgetRequest{Content: &WidgetContent{Style: WidgetStyleBold}}, []string{`"style":"bold"`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(c.body)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got := string(b)
			for _, key := range []string{`"style":`, `"style_by_size":`, `"density":`, `"density_by_size":`} {
				wanted := false
				for _, w := range c.want {
					wanted = wanted || strings.HasPrefix(w, key)
				}
				if !wanted && strings.Contains(got, key) {
					t.Errorf("body = %s, want no %s key", got, key)
				}
			}
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("body = %s, want %s", got, w)
				}
			}
		})
	}
}

func TestValidateWidgetLooks(t *testing.T) {
	templates := []WidgetTemplate{
		WidgetTemplateValue, WidgetTemplateProgress, WidgetTemplateStatus, WidgetTemplateGauge,
		WidgetTemplateStatList, WidgetTemplateTrend, WidgetTemplateCountdown, WidgetTemplateBattery,
		WidgetTemplateSchedule, WidgetTemplateFlow, WidgetTemplateHeatmap,
	}
	for _, tmpl := range templates {
		for _, s := range []WidgetStyle{"", WidgetStyleStandard, WidgetStyleBold, WidgetStyleClassic, "Classic"} {
			c := WidgetContent{Style: s, StyleBySize: map[string]WidgetStyle{"small": s, "lock_screen": WidgetStyleClassic}}
			if s == "" {
				c.StyleBySize = nil
			}
			if err := ValidateWidgetLooks(tmpl, c); err != nil {
				t.Errorf("%s/%q: %v", tmpl, s, err)
			}
		}
	}

	cases := []struct {
		name    string
		tmpl    WidgetTemplate
		content WidgetContent
		wantErr string // "" = valid
	}{
		{"nothing set", WidgetTemplateValue, WidgetContent{}, ""},
		{"empty maps on value", WidgetTemplateValue, WidgetContent{StyleBySize: map[string]WidgetStyle{}, DensityBySize: map[string]string{}}, ""},
		{"every size", WidgetTemplateGauge, WidgetContent{StyleBySize: map[string]WidgetStyle{"small": "bold", "medium": "classic", "large": "standard", "lock_screen": "bold"}}, ""},
		{"density on stat_list", WidgetTemplateStatList, WidgetContent{Density: WidgetDensityAuto, DensityBySize: map[string]string{"medium": "comfortable", "large": "compact"}}, ""},
		{"mixed case", WidgetTemplateStatList, WidgetContent{Style: "Bold", StyleBySize: map[string]WidgetStyle{"Lock_Screen": "CLASSIC"}, Density: "Compact", DensityBySize: map[string]string{"SMALL": "Comfortable"}}, ""},

		{"unknown style", WidgetTemplateStatus, WidgetContent{Style: "Fancy"}, `style must be one of standard, bold, classic, got "fancy"`},
		// The names from before the rename. Only the server maps them, for one
		// published client; here they are as unknown as any other word.
		{"pre-rename refined", WidgetTemplateValue, WidgetContent{Style: "refined"}, `style must be one of standard, bold, classic, got "refined"`},
		{"pre-rename rethink", WidgetTemplateValue, WidgetContent{StyleBySize: map[string]WidgetStyle{"small": "Rethink"}}, `style_by_size.small must be one of standard, bold, classic, got "rethink"`},
		{"pre-rename original", WidgetTemplateStatus, WidgetContent{Style: "ORIGINAL"}, `style must be one of standard, bold, classic, got "original"`},
		{"style_by_size unknown key", WidgetTemplateValue, WidgetContent{StyleBySize: map[string]WidgetStyle{"Tiny": "bold"}}, `style_by_size key must be one of small, medium, large, lock_screen, got "tiny"`},
		{"style_by_size unknown value", WidgetTemplateValue, WidgetContent{StyleBySize: map[string]WidgetStyle{"large": "Retro"}}, `style_by_size.large must be one of standard, bold, classic, got "retro"`},
		{"style_by_size key twice", WidgetTemplateValue, WidgetContent{StyleBySize: map[string]WidgetStyle{"small": "bold", "Small": "bold"}}, `style_by_size names small twice`},
		{"unknown density", WidgetTemplateStatList, WidgetContent{Density: "Dense"}, `density must be one of auto, compact, comfortable, got "dense"`},
		{"density_by_size unknown key", WidgetTemplateStatList, WidgetContent{DensityBySize: map[string]string{"watch": "auto"}}, `density_by_size key must be one of small, medium, large, lock_screen, got "watch"`},
		{"density_by_size unknown value", WidgetTemplateStatList, WidgetContent{DensityBySize: map[string]string{"lock_screen": "tight"}}, `density_by_size.lock_screen must be one of auto, compact, comfortable, got "tight"`},
		{"density on value", WidgetTemplateValue, WidgetContent{Density: WidgetDensityCompact}, `density is only available for template "stat_list", not "value"`},
		{"density_by_size on gauge", WidgetTemplateGauge, WidgetContent{DensityBySize: map[string]string{"small": "compact"}}, `density_by_size is only available for template "stat_list", not "gauge"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateWidgetLooks(c.tmpl, c.content)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != c.wantErr {
				t.Fatalf("err = %v, want %q", err, c.wantErr)
			}
		})
	}
}
