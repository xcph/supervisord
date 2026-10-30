package types

import "testing"

func TestSummarizeProcessInfos(t *testing.T) {
	infos := []ProcessInfo{
		{Name: "a", Statename: "RUNNING"},
		{Name: "b", Statename: "RUNNING"},
		{Name: "c", Statename: "STOPPED"},
		{Name: "d", Statename: "FATAL"},
		{Name: "e", Statename: "EXITED"},
	}
	s := SummarizeProcessInfos(infos)
	if s.Total != 5 || s.Running != 2 || s.Stopped != 1 || s.Fatal != 1 || s.Exited != 1 {
		t.Fatalf("%+v", s)
	}
	got := s.Format()
	want := "5 programs: 2 running, 1 stopped, 1 exited, 1 fatal"
	if got != want {
		t.Fatalf("Format=%q want %q", got, want)
	}
}

func TestSummarizeEmpty(t *testing.T) {
	if got := SummarizeProcessInfos(nil).Format(); got != "0 programs" {
		t.Fatal(got)
	}
}
