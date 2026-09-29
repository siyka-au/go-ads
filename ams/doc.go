// Package ams is the vocabulary of the Beckhoff AMS/ADS protocol: addresses,
// ports, commands, index groups, return codes, device states, data types and the
// AMS header codec.
//
// It does no I/O. The adsclient package speaks the protocol over TCP, and the
// root ads package builds a reconnecting, symbol-aware Session on top of it.
package ams
