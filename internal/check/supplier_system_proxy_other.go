//go:build !darwin && !windows

package check

func readSupplierSystemProxy() supplierSystemProxy { return supplierSystemProxy{} }
