# frozen_string_literal: true
#
# Usage of PStore — the transactional, Marshal-backed object store this library
# backs. Every read or write happens inside a transaction: the store loads the
# table, runs the block, and commits on normal exit (or aborts early). Runs under
# go-embedded-ruby (rbgo); see examples/README.md.

require "pstore"

db = PStore.new("/tmp/pstore_usage.pstore")
p db.path                                 # => "/tmp/pstore_usage.pstore"

# A read-write transaction: assignments commit when the block returns normally.
db.transaction do
  db[:fruits] = ["apple", "pear"]         # store an Array under a Symbol root
  db["count"] = 2                         # roots can be any Marshal-able key
end

# A read-only transaction (pass true): #[], #fetch and #roots work, writes raise.
db.transaction(true) do
  p db.roots                              # => [:fruits, "count"]  (the root keys)
  p db[:fruits]                           # => ["apple", "pear"]
  p db.fetch("count")                     # => 2
  p db.root?(:fruits)                     # => true
  p db.fetch(:missing, "n/a")             # => "n/a"  (default for an absent key)
end

# #abort discards every change made in the block and stops it immediately.
db.transaction do
  db.delete("count")
  db.abort                                # roll back: "count" is kept on disk
end

db.transaction(true) { p db.key?("count") } # => true  (the abort rolled back)
