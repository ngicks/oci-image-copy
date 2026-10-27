// Package ociimagecopy copies image sets between an image store and containers-storage.
// It composes the store, transfer, cstorage and registry packages behind one Copy and
// one List call, and owns the configuration the oci-image-copy CLI loads.
package ociimagecopy
