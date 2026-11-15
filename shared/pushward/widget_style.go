package pushward

import (
	"fmt"
	"slices"
	"strings"
)

// WidgetStyle is the look a widget template is drawn in; every template offers
// all three. A style sent through the API wins over the one the user picked for
// that widget in the app; empty leaves the choice to the user, and a widget
// nobody picked a style for is drawn Standard. WidgetContent.StyleBySize does
// the same for one size, and Density / DensityBySize for the stat_list row
// density.
//
// CreateWidget is an upsert that replaces the whole content, so a create
// without these fields clears what an earlier one sent: a producer that
// re-creates its widgets has to send them every time to keep them. UpdateWidget
// leaves an empty field out and the server keeps the stored one; as with the
// other omitempty fields, this client cannot send the null that clears it on
// PATCH.
type WidgetStyle string

const (
	WidgetStyleStandard WidgetStyle = "standard"
	WidgetStyleBold     WidgetStyle = "bold"
	WidgetStyleClassic  WidgetStyle = "classic"
)

// Row densities of the stat_list template, the values of WidgetContent.Density
// and DensityBySize.
const (
	WidgetDensityAuto        = "auto"
	WidgetDensityCompact     = "compact"
	WidgetDensityComfortable = "comfortable"
)

var (
	widgetStyles    = []WidgetStyle{WidgetStyleStandard, WidgetStyleBold, WidgetStyleClassic}
	widgetDensities = []string{WidgetDensityAuto, WidgetDensityCompact, WidgetDensityComfortable}
	// widgetSizes are the keys of StyleBySize and DensityBySize: the three Home
	// Screen sizes and lock_screen for every accessory family.
	widgetSizes = []string{"small", "medium", "large", "lock_screen"}
)

// ValidateWidgetLooks checks Style, StyleBySize, Density and DensityBySize of
// content for a widget on template t the way pushward-server does, with the
// message its 422 carries. Any template takes any style; the density fields
// are stat_list only. Case is ignored, as on the server.
func ValidateWidgetLooks(t WidgetTemplate, c WidgetContent) error {
	if c.Style != "" {
		if err := oneOf("style", c.Style, widgetStyles); err != nil {
			return err
		}
	}
	if err := validateBySize("style_by_size", c.StyleBySize, widgetStyles); err != nil {
		return err
	}
	if c.Density != "" {
		if err := oneOf("density", c.Density, widgetDensities); err != nil {
			return err
		}
	}
	if err := validateBySize("density_by_size", c.DensityBySize, widgetDensities); err != nil {
		return err
	}
	if t != WidgetTemplateStatList {
		if c.Density != "" {
			return fmt.Errorf("density is only available for template %q, not %q", WidgetTemplateStatList, t)
		}
		if len(c.DensityBySize) > 0 {
			return fmt.Errorf("density_by_size is only available for template %q, not %q", WidgetTemplateStatList, t)
		}
	}
	return nil
}

func oneOf[V ~string](field string, v V, allowed []V) error {
	v = V(strings.ToLower(string(v)))
	if slices.Contains(allowed, v) {
		return nil
	}
	names := make([]string, len(allowed))
	for i, a := range allowed {
		names[i] = string(a)
	}
	return fmt.Errorf("%s must be one of %s, got %q", field, strings.Join(names, ", "), v)
}

// validateBySize checks a per-size map. Keys are folded too, so "Small" and
// "small" in one map name the same size twice.
func validateBySize[V ~string](field string, m map[string]V, allowed []V) error {
	seen := make(map[string]bool, len(m))
	for k, v := range m {
		size := strings.ToLower(k)
		if err := oneOf(field+" key", size, widgetSizes); err != nil {
			return err
		}
		if seen[size] {
			return fmt.Errorf("%s names %s twice", field, size)
		}
		seen[size] = true
		if err := oneOf(field+"."+size, v, allowed); err != nil {
			return err
		}
	}
	return nil
}
