// Command watch_events streams typed template events from an Ootle indexer over SSE
// (GET /transactions/events/stream) and prints each event as it arrives. It is a
// read-only example — it never funds, signs, or submits anything, so it is safe to run
// against any indexer.
//
// It drives ootle.Client.WatchEvents, which reconnects automatically (resuming from the
// last seen event id via the Last-Event-ID header) and surfaces transient connection
// problems as non-fatal warnings on its error channel.
//
// Configuration is read from the environment:
//
//	OOTLE_INDEXER_URL        indexer REST base URL (default transport.DefaultBaseURL)
//	OOTLE_COMPONENT_ADDRESS  filter by emitting substate id, e.g. component_<hex>
//	                         (optional; empty ⇒ all components)
//	OOTLE_EVENT_TOPIC        filter by exact event topic, e.g. std.vault.withdraw
//	                         (optional; empty ⇒ all topics)
//
// Run it (the module imports ootle → internal/cffi, so the native lib must be vendored;
// run `make native` once if it is not):
//
//	go run ./examples/watch_events
//
// Then, from another terminal, drive a transaction (e.g. a faucet claim or a
// counter.increase()) and watch matching event frames print here. Press Ctrl-C to stop;
// the watch is cancelled cleanly and the program exits.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/tari-project/ootle-go/examples/internal/common"
	"github.com/tari-project/ootle-go/ootle"
)

func main() {
	env := common.LoadEnv()
	filter := ootle.EventFilter{
		SubstateID: os.Getenv("OOTLE_COMPONENT_ADDRESS"),
		Topic:      os.Getenv("OOTLE_EVENT_TOPIC"),
	}

	client := common.NewClient(env)

	// Ctrl-C cancels the context, which ends the watch cleanly (both channels close,
	// no terminal error). signal.NotifyContext restores the default signal handler on
	// the second interrupt, so a second Ctrl-C force-quits if shutdown ever hangs.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	log.Printf("watching events on %s (component=%q topic=%q); press Ctrl-C to stop",
		env.IndexerURL, filter.SubstateID, filter.Topic)

	events, errs := client.WatchEvents(ctx, filter)
	for events != nil || errs != nil {
		select {
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			printEvent(ev)
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			// Reconnect warnings and the single terminal error (e.g. a transport that
			// cannot stream) both arrive here.
			log.Printf("watch error: %v", err)
		}
	}

	log.Print("watch ended")
}

// printEvent renders one event in a compact, human-readable line.
func printEvent(ev ootle.Event) {
	fmt.Printf("event id=%d topic=%q substate=%q tx=%s\n",
		ev.ID, ev.Topic, ev.SubstateID, ev.TransactionID)
	for k, v := range ev.Payload {
		fmt.Printf("    %s = %s\n", k, v)
	}
}
