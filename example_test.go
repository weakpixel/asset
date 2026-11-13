package asset_test

import (
	"fmt"
	"strings"
	"testing/fstest"

	"github.com/weakpixel/asset"
)

// Greeting is a stand-in for an asset type. A real one would hold a
// GPU texture or similar, created in Finish.
type Greeting struct{ Text string }

// greetingLoader decodes a file into upper-case text. Decode is the part that
// would run on a worker; Finish builds the final value on the game goroutine.
type greetingLoader struct{}

func (greetingLoader) Decode(_ *asset.DecodeContext, data []byte) (string, error) {
	return strings.ToUpper(string(data)), nil
}

func (greetingLoader) Finish(_ *asset.FinishContext, s string) (Greeting, error) {
	return Greeting{Text: s}, nil
}

func (greetingLoader) Unload(*Greeting) {}

func Example() {
	srv := asset.NewServer(asset.Options{
		FS: fstest.MapFS{"hello.txt": {Data: []byte("hello")}},
		// Workers: runtime.GOMAXPROCS(0) - 1 would decode in the background;
		// 0 decodes inside Update, which keeps this example deterministic.
		Workers: 0,
	})
	defer srv.Close()

	greetings := asset.Register(srv, greetingLoader{}, Greeting{Text: "(loading)"})

	// Load returns at once. The handle is what components store.
	level := srv.NewScope("level-1")
	hello := asset.Load[Greeting](level, "hello.txt")
	fmt.Println("before Update:", greetings.Get(hello).Text)

	// Update runs first in every tick: it decodes, commits and unloads.
	for ev := range srv.Update().All() {
		fmt.Println("event:", ev.Key, ev.Kind == asset.EventLoaded)
	}
	fmt.Println("after Update:", greetings.Get(hello).Text)

	// Releasing the scope unloads what nothing else holds. The old handle is
	// not dangling: it resolves to the placeholder.
	level.Release()
	srv.Update()
	fmt.Println("after Release:", greetings.Get(hello).Text)

	// Output:
	// before Update: (loading)
	// event: hello.txt true
	// after Update: HELLO
	// after Release: (loading)
}

func ExampleLoadSync() {
	srv := asset.NewServer(asset.Options{FS: fstest.MapFS{"boot.txt": {Data: []byte("boot")}}})
	defer srv.Close()
	greetings := asset.Register(srv, greetingLoader{}, Greeting{Text: "(loading)"})

	// Blocks until the asset is Ready or Failed. For boot screens and tools.
	h, err := asset.LoadSync[Greeting](srv.Global(), "boot.txt")
	fmt.Println(greetings.Get(h).Text, err)

	// Output:
	// BOOT <nil>
}
