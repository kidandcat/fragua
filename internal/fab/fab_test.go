package fab

import "testing"

func TestJlcpcbProfileStandardVia(t *testing.T) {
	p, err := ProfileByName("jlcpcb")
	if err != nil {
		t.Fatal(err)
	}
	if p.MinDrillMM != 0.30 || p.MinViaDiameterMM != 0.60 || p.MinAnnularRingMM < 0.15 {
		t.Fatalf("jlcpcb standard via: %+v", p)
	}
	via02, err := ProfileByName("jlcpcb-2l-via02")
	if err != nil {
		t.Fatal(err)
	}
	if via02.MinDrillMM != 0.20 {
		t.Fatalf("via02 opt-in: %+v", via02)
	}
	l4, err := ProfileByName("jlcpcb-4l")
	if err != nil {
		t.Fatal(err)
	}
	if l4.MinDrillMM != 0.30 || l4.MinViaDiameterMM != 0.60 {
		t.Fatalf("jlcpcb-4l via: %+v", l4)
	}
}
