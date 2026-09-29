// Package symtab models a PLC's symbol table: it parses the symbol and data
// type uploads into a tree of Symbols, and converts between a symbol's bytes
// and its Go value (Decode/Encode).
//
// It does no I/O and knows nothing of sessions; the root ads package owns the
// cache, the locking and the connection.
package symtab
