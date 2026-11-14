package ringpalette

import "testing"

func TestParseGradientAndSample(t *testing.T) {
	g, err := Parse("linear-gradient(90deg, #FF0000 0%, #0000FF 100%)")
	if err != nil {
		t.Fatal(err)
	}
	if got := g.Sample(0); got.R != 255 || got.B != 0 {
		t.Fatalf("start = %+v", got)
	}
	if got := g.Sample(1); got.B != 255 || got.R != 0 {
		t.Fatalf("end = %+v", got)
	}
	mid := g.Sample(0.5)
	if mid.R < 120 || mid.R > 136 || mid.B < 120 || mid.B > 136 {
		t.Fatalf("mid = %+v", mid)
	}
}

func TestParseSolidAndImplicitPositions(t *testing.T) {
	solid, err := Parse("#00FF80")
	if err != nil {
		t.Fatal(err)
	}
	if len(solid.Stops) != 1 || solid.Solid().G != 255 {
		t.Fatalf("solid = %+v", solid)
	}
	g, err := Parse("linear-gradient(#FF0000, #00FF00, #0000FF)")
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Stops) != 3 || g.Stops[0].Position != 0 || g.Stops[1].Position != 0.5 || g.Stops[2].Position != 1 {
		t.Fatalf("positions = %+v", g.Stops)
	}
	if got := g.Format(); got != "linear-gradient(90deg, #FF0000 0%, #00FF00 50%, #0000FF 100%)" {
		t.Fatalf("canonical = %q", got)
	}
}

func TestParseRejectsInvalidPalettes(t *testing.T) {
	for _, in := range []string{
		"", "aurora", "linear-gradient(red, blue)", "linear-gradient(90deg, #GG0000)",
		"linear-gradient(90deg, #FF0000 50%, #00FF00 25%)", "linear-gradient(400deg, #FF0000)",
		"radial-gradient(#FF0000, #0000FF)",
	} {
		if _, err := Parse(in); err == nil {
			t.Fatalf("Parse(%q) accepted an invalid palette", in)
		}
	}
}
