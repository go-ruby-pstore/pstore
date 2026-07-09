# Ruby examples

Pure-Ruby examples of `pstore` — the transactional, `Marshal`-backed object store
this library backs. They run under [go-embedded-ruby](https://github.com/go-embedded-ruby/ruby)
(rbgo) via the `require "pstore"` binding.

```sh
rbgo examples/pstore_usage.rb
```

| File | Shows |
| --- | --- |
| [`pstore_usage.rb`](pstore_usage.rb) | Opening a store with `PStore.new`, `#path`, a read-write `#transaction` that commits on normal exit (`#[]=`), a read-only transaction (`#roots`, `#[]`, `#fetch` with a default, `#root?`), and `#abort` rolling back a `#delete`. |
