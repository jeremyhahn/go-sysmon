### Fixed

- **Concurrent history requests share one lookback fetch in every
  interleaving.** A request that found the lookback cache empty and took the
  fetch only after another request's fetch had finished asked the database a
  second time for the answer already cached. It now reads the cache again once
  it holds the fetch.
