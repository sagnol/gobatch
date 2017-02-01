package source

import "context"

type channelSource struct {
	items <-chan interface{}
}

func Channel(items <-chan interface{}) Source {
	return &channelSource{
		items: items,
	}
}

func (s *channelSource) Read(ctx context.Context, items chan<- interface{}, errs chan<- error) {
	defer close(items)
	defer close(errs)

	for {
		if item, ok := <-s.items; ok {
			items <- item
		} else {
			return
		}
	}
}
