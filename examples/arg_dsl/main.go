// Command arg_dsl demonstrates the typed argument DSL for signed integers,
// non-fungible ids, and composite (list / optional) arguments. It builds a
// template-call instruction, marshals it, and prints the resulting wire JSON.
//
// It is a read-only example — it never funds, signs, or submits anything, and
// needs no indexer, keys, or identity, so it runs entirely offline:
//
//	go run ./examples/arg_dsl
//
// Each constructor maps to one externally-tagged wire shape:
//
//	ArgI64(-1)                              => {"I64":-1}
//	ArgNonFungibleID(NonFungibleU32(7))     => {"NonFungibleId":"u32_7"}
//	ArgList(a, b)                           => {"List":[a, b]}
//	ArgList()                               => {"List":[]}
//	ArgSome(inner)                          => {"Optional":inner}
//	ArgNone()                               => {"Optional":null}
//
// The NonFungible{U32,U64,String,UUID} helpers produce the canonical id string
// that ArgNonFungibleID wraps. The template name and addresses below are
// illustrative placeholders — nothing is sent, so they are never resolved.
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tari-project/ootle-go/ootle"
)

// Run builds one template-call instruction exercising every new argument arm
// and prints its wire JSON.
func Run(ctx context.Context) error {
	call := ootle.CallFunction("template_demo", "mint",
		ootle.ArgI64(-1),
		ootle.ArgNonFungibleID(ootle.NonFungibleU32(7)),
		ootle.ArgList(
			ootle.ArgNonFungibleID(ootle.NonFungibleU64(8)),
			ootle.ArgNonFungibleID(ootle.NonFungibleString("vip")),
			ootle.ArgNonFungibleID(ootle.NonFungibleUUID([32]byte{})),
		),
		ootle.ArgList(), // empty list
		ootle.ArgSome(ootle.ArgAmount(1_000_000)),
		ootle.ArgNone(),
		ootle.ArgAddress("vault_dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"),
	)

	out, err := json.MarshalIndent(call, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal call: %w", err)
	}
	fmt.Printf("mint instruction:\n%s\n", out)
	return nil
}

func main() {
	if err := Run(context.Background()); err != nil {
		panic(err)
	}
}
