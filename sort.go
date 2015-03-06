package main

type VideoSorter struct {
	Slice []Video
	Cmp   func(a, b Video) bool
}

func (s VideoSorter) Len() int {
	return len(s.Slice)
}

func (s VideoSorter) Less(i, j int) bool {
	return s.Cmp(s.Slice[i], s.Slice[j])
}

func (s VideoSorter) Swap(i, j int) {
	s.Slice[i], s.Slice[j] = s.Slice[j], s.Slice[i]
}
