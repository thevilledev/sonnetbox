package sonnetbox_test

import (
	"context"
	"fmt"

	"github.com/thevilledev/sonnetbox"
)

func Example() {
	ctx := context.Background()
	engine, err := sonnetbox.NewEngine(ctx, sonnetbox.EngineConfig{})
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := engine.Close(context.Background()); err != nil {
			panic(err)
		}
	}()

	result, err := engine.Evaluate(ctx, sonnetbox.Request{
		Filename: "main.jsonnet",
		Source:   `{answer: 6 * 7}`,
	})
	if err != nil {
		panic(err)
	}
	fmt.Print(string(result.Output))

	// Output:
	// {
	//    "answer": 42
	// }
}
