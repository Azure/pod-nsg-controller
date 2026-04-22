package model

// OwnershipKey generates a deterministic ownership key from cluster, namespace, and mapping name.
func OwnershipKey(clusterName, namespace, mappingName string) string {
	return clusterName + "-" + namespace + "-" + mappingName
}
