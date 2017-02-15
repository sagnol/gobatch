package batch

import (
	"context"
	"sync"
	"time"
)

// Batch provides batch processing given an Source and a Processor. Data is
// read from the Source and processed in batches by the Processor. Any errors
// are wrapped in either a SourceError or a ProcessorError, so the caller
// can determine where the errors came from.
//
// To create a new Batch, call the New function. Creating one using &Batch{}
// will return the default Batch.
//
//    // The following are equivalent
//    defaultBatch1 := &gobatch.Batch{}
//    defaultBatch2 := gobatch.New(nil)
//    defaultBatch3 := gobatch.New(ConstantConfig(&ConstantConfig()))
//
// The defaults (with nil Config) provide a usable, but likely suboptimal, Batch
// where items are processed as soon as they are retrieved from the source. Reading
// is done by a single goroutine, and processing is done in the background using as
// many goroutines as necessary with no limit.
//
// This is a simplified version of how the default Batch works:
//
//    itemsCh := make(chan item.Item)
//    go source.Read(ctx, srcCh, itemsCh, errsCh)
//    for item := range itemsCh {
//      go processor.Process(ctx, []processor.Item{item, errsCh)
//    }
//
// Batch runs asynchronously until the source closes its write channels, signaling
// that there is nothing else to process. Once that happens, and the pipeline has
// been drained (all items have been processed), there are two ways for the
// caller to know: the error channel returned from Go is closed, or the channel
// returned from Done is closed.
//
// The first way can be used if errors need to be processed. A simple loop
// could look like this:
//
//    errs := batch.Go(ctx, s, p)
//    for err := range errs {
//      // Log the error here...
//      log.Print(err.Error())
//    }
//    // Now batch processing is done
//
// If the errors don't need to be processed, the IgnoreErrors function can be
// used to drain the error channel. Then the Done channel can be used to
// determine whether or not batch processing is complete:
//
//    IgnoreErrors(batch.Go(ctx, s, p))
//    <-batch.Done()
//    // Now batch processing is done
//
// Note that the errors returned on the error channel may be wrapped in a
// BatchError so the caller knows whether they come from the source or the
// processor (or neither). Errors from the source will be of type SourceError,
// and errors from the processor will be of type ProcessorError. Errors from
// Batch itself will be neither.
type Batch struct {
	config Config

	src   Source
	proc  Processor
	items chan *Item
	ids   chan uint64 // For unique IDs
	done  chan struct{}

	// mu protects the following variables. The reason errs is protected is
	// to avoid sending on a closed channel in the Go method.
	mu      sync.Mutex
	running bool
	errs    chan error
}

// New creates a new Batch based on specified config. If config is nil,
// the default config is used as described in Batch.
func New(config Config) *Batch {
	return &Batch{
		config: config,
	}
}

// Source reads items that are to be batch processed.
type Source interface {
	// Read reads items from somewhere and writes them to the items
	// channel. Any errors it encounters while reading are written to the
	// errs channel. The in channel provides a steady stream of Items that
	// have pre-set data so the batch processor can identify them. A helper
	// function, NextItem, is provided to retrieve an item from the channel,
	// set it, and return it:
	//
	//    items <- batch.NextItem(in, myData)
	//
	// Read is only run in a single goroutine. It can spawn as many are
	// necessary for reading.
	//
	// Once reading is finished (or when the program ends) both items and
	// errs need to be closed. This signals to Batch that it should drain
	// the pipeline and finish. It is not enough for Read to return.
	//
	//    func (s source) Read(ctx context.Context, in <-chan *Item, items chan<- *Item, errs chan<- error) {
	//      defer close(items)
	//      defer close(errs)
	//      // Read items until done...
	//    }
	//
	// Read should not modify an item after adding it to items.
	Read(ctx context.Context, in <-chan *Item, items chan<- *Item, errs chan<- error)
}

// Processor processes items in batches.
type Processor interface {
	// Process processes items and returns any errors on the errs channel.
	// When it is done, it must close the errs channel to signify that it's
	// finished processing. Simply returning isn't enough.
	//
	//    func (p *processor) Process(ctx context.Context, items []interface{}, errs chan<- error) {
	//      defer close(errs)
	//      // Do processing here...
	//    }
	//
	// Batch does not wait for Process to finish, so it can spawn a
	// goroutine and then return, as long as errs is closed at the end.
	//
	//    // This is ok
	//    func (p *processor) Process(ctx context.Context, items []interface{}, errs chan<- error) {
	//      go func() {
	//        defer close(errs)
	//        time.Sleep(time.Second)
	//        fmt.Println(items)
	//      }()
	//    }
	//
	// Process may be run in any number of concurrent goroutines. If
	// concurrency needs to be limited it must be done in Process; for
	// example, by using a semaphore channel.
	Process(ctx context.Context, items []*Item, errs chan<- error)
}

// Go starts batch processing asynchronously and returns a channel on
// which errors are written. When processing is done and the pipeline
// is drained, the error channel is closed.
//
// Even though Go has several goroutines running concurrently, concurrent
// calls to Go are not allowed. If Go is called before a previous call
// completes, the second one will panic.
//
//    // NOTE: bad - this will panic!
//    errs := batch.Go(ctx, s, p)
//    errs2 := batch.Go(ctx, s, p) // this call panics
//
// Note that Go does not stop if ctx is done. Otherwise loss of data could occur.
// Suppose the source reads item A and then ctx is canceled. If Go were to return
// right away, item A would not be processed and it would be lost.
//
// To avoid situations like that, a proper way to handle context completion
// is for the source to check for ctx done and then close its channels. The
// batch processor realizes the source is finished reading items and it sends
// all remaining items to the processor for processing. Once the processor is
// done, it closes its error channel to signal to the batch processor.
// Finally, the batch processor signals to its caller that processing is
// complete and the entire pipeline is drained.
func (b *Batch) Go(ctx context.Context, s Source, p Processor) <-chan error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.running {
		panic("Concurrent calls to Batch.Go are not allowed")
		return nil
	}

	if b.config == nil {
		b.config = ConstantConfig(nil)
	}

	b.running = true
	b.errs = make(chan error)

	b.src = s
	b.proc = p
	b.items = make(chan *Item)
	b.ids = make(chan uint64)
	b.done = make(chan struct{})

	go b.doIDGenerator()
	go b.doReader(ctx)
	go b.doProcessors(ctx)

	return b.errs
}

// Done provides an alternative way to determine when processing is
// complete. When it is, the channel is closed, signaling that everything
// is done.
func (b *Batch) Done() <-chan struct{} {
	return b.done
}

// doIDGenerator generates unique IDs for the items in the pipeline.
func (b *Batch) doIDGenerator() {
	for id := uint64(0); ; id++ {
		select {
		case b.ids <- id:

		case <-b.done:
			return
		}
	}
}

// doReader starts the reader goroutine and reads from its channels.
func (b *Batch) doReader(ctx context.Context) {
	itemsIn := make(chan *Item)
	itemsOut := make(chan *Item)
	errs := make(chan error)

	go b.src.Read(ctx, itemsIn, itemsOut, errs)

	nextItem := &Item{
		id: <-b.ids,
	}

	var itemsClosed, errsClosed bool
	for !itemsClosed || !errsClosed {
		select {
		case itemsIn <- nextItem:
			nextItem = &Item{
				id: <-b.ids,
			}

		case item, ok := <-itemsOut:
			if ok {
				b.items <- item
			} else {
				itemsClosed = true
			}

		case err, ok := <-errs:
			if ok {
				b.errs <- newSourceError(err)
			} else {
				errsClosed = true
			}
		}
	}

	close(b.items)
}

// doProcessors starts the processor goroutine.
func (b *Batch) doProcessors(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		b.process(ctx)
	}()
	wg.Wait()

	// Once processors are complete, everything is
	b.mu.Lock()
	close(b.errs)
	close(b.done)
	b.running = false
	b.mu.Unlock()
}

func (b *Batch) process(ctx context.Context) {
	var (
		wg      sync.WaitGroup
		done    bool
		bufSize uint64
	)

	// Process one batch each time
	for !done {
		config := b.config.Get()

		if config.MaxTime > 0 && config.MinTime > 0 && config.MaxTime < config.MinTime {
			config.MinTime = config.MaxTime
		}
		if config.MaxItems > 0 && config.MinItems > 0 && config.MaxItems < config.MinItems {
			config.MinItems = config.MaxItems
		}

		// TODO smarter buffer size (perhaps from the config)
		if config.MaxItems > 0 {
			bufSize = config.MaxItems
		} else if config.MinItems > 0 {
			bufSize = config.MinItems * 2
		} else {
			bufSize = 1024
		}

		var items = make([]*Item, 0, bufSize)
		done, items = b.waitForItems(ctx, items, &config)

		// TODO this resets the time whenever no items are available. Need to
		// decide if that's the right way to do it.
		if len(items) == 0 {
			continue
		}

		// Process all current items
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs := make(chan error)
			go b.proc.Process(ctx, items, errs)
			for err := range errs {
				b.errs <- newProcessorError(err)
			}
		}()
	}

	// Wait for all processing to complete
	wg.Wait()
}

// waitForItems waits until enough items are read to begin batch processing, based
// on config. It returns true if processing is completely finished, and false
// otherwise.
func (b *Batch) waitForItems(ctx context.Context, items []*Item, config *ConfigValues) (bool, []*Item) {
	var (
		reachedMinTime bool
		itemsRead      uint64

		minTimer <-chan time.Time
		maxTimer <-chan time.Time
	)

	// Be careful not to set timers that end right away. Instead, if a
	// min or max time is not specified, make a timer channel that's never
	// written to.
	if config.MinTime > 0 {
		minTimer = time.After(config.MinTime)
	} else {
		minTimer = make(chan time.Time)
		reachedMinTime = true
	}

	if config.MaxTime > 0 {
		maxTimer = time.After(config.MaxTime)
	} else {
		maxTimer = make(chan time.Time)
	}

	for {
		select {
		case item, ok := <-b.items:
			if ok {
				items = append(items, item)
				itemsRead++
				if itemsRead >= config.MinItems && reachedMinTime {
					return false, items
				}
				if config.MaxItems > 0 && itemsRead >= config.MaxItems {
					return false, items
				}
			} else {
				// Finished processing
				return true, items
			}

		case <-minTimer:
			reachedMinTime = true
			if itemsRead >= config.MinItems {
				return false, items
			}

		case <-maxTimer:
			return false, items
		}
	}
}