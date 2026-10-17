package eventstore

// ShareLog makes dst read and append to the log of src, so the contract
// suite can open stores with different Codecs over one log.
func ShareLog(dst, src *InMemoryStore) {
	dst.log = src.log
}
