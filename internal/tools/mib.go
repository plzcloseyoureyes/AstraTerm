package tools

import (
	"sort"
	"strings"
)

// mibNames is a compact embedded subset of the standard MIBs (SNMPv2-MIB, IF-MIB, IP-MIB, TCP/UDP-MIB,
// HOST-RESOURCES-MIB, ENTITY-MIB, LLDP-MIB, UCD-SNMP-MIB) covering the objects operators query most (RESEARCH CC-4
// "embedded core MIBs"). It maps names in both directions: "sysDescr.0" / "ifTable" in the OID field, and returned
// OIDs are labelled ("ifDescr.3"). Unknown OIDs stay numeric.
var mibNames = map[string]string{
	"iso": "1", "org": "1.3", "dod": "1.3.6", "internet": "1.3.6.1", "mgmt": "1.3.6.1.2", "mib-2": "1.3.6.1.2.1",
	"private": "1.3.6.1.4", "enterprises": "1.3.6.1.4.1", "snmpV2": "1.3.6.1.6",

	// SNMPv2-MIB system group
	"system": "1.3.6.1.2.1.1", "sysDescr": "1.3.6.1.2.1.1.1", "sysObjectID": "1.3.6.1.2.1.1.2",
	"sysUpTime": "1.3.6.1.2.1.1.3", "sysContact": "1.3.6.1.2.1.1.4", "sysName": "1.3.6.1.2.1.1.5",
	"sysLocation": "1.3.6.1.2.1.1.6", "sysServices": "1.3.6.1.2.1.1.7", "sysORLastChange": "1.3.6.1.2.1.1.8",
	"sysORTable": "1.3.6.1.2.1.1.9", "sysORDescr": "1.3.6.1.2.1.1.9.1.3",

	// IF-MIB
	"interfaces": "1.3.6.1.2.1.2", "ifNumber": "1.3.6.1.2.1.2.1", "ifTable": "1.3.6.1.2.1.2.2",
	"ifEntry": "1.3.6.1.2.1.2.2.1", "ifIndex": "1.3.6.1.2.1.2.2.1.1", "ifDescr": "1.3.6.1.2.1.2.2.1.2",
	"ifType": "1.3.6.1.2.1.2.2.1.3", "ifMtu": "1.3.6.1.2.1.2.2.1.4", "ifSpeed": "1.3.6.1.2.1.2.2.1.5",
	"ifPhysAddress": "1.3.6.1.2.1.2.2.1.6", "ifAdminStatus": "1.3.6.1.2.1.2.2.1.7",
	"ifOperStatus": "1.3.6.1.2.1.2.2.1.8", "ifLastChange": "1.3.6.1.2.1.2.2.1.9", "ifInOctets": "1.3.6.1.2.1.2.2.1.10",
	"ifInUcastPkts": "1.3.6.1.2.1.2.2.1.11", "ifInNUcastPkts": "1.3.6.1.2.1.2.2.1.12",
	"ifInDiscards": "1.3.6.1.2.1.2.2.1.13", "ifInErrors": "1.3.6.1.2.1.2.2.1.14",
	"ifInUnknownProtos": "1.3.6.1.2.1.2.2.1.15", "ifOutOctets": "1.3.6.1.2.1.2.2.1.16",
	"ifOutUcastPkts": "1.3.6.1.2.1.2.2.1.17", "ifOutNUcastPkts": "1.3.6.1.2.1.2.2.1.18",
	"ifOutDiscards": "1.3.6.1.2.1.2.2.1.19", "ifOutErrors": "1.3.6.1.2.1.2.2.1.20", "ifOutQLen": "1.3.6.1.2.1.2.2.1.21",
	"ifMIB": "1.3.6.1.2.1.31", "ifXTable": "1.3.6.1.2.1.31.1.1", "ifXEntry": "1.3.6.1.2.1.31.1.1.1",
	"ifName": "1.3.6.1.2.1.31.1.1.1.1", "ifInMulticastPkts": "1.3.6.1.2.1.31.1.1.1.2",
	"ifInBroadcastPkts": "1.3.6.1.2.1.31.1.1.1.3", "ifHCInOctets": "1.3.6.1.2.1.31.1.1.1.6",
	"ifHCInUcastPkts": "1.3.6.1.2.1.31.1.1.1.7", "ifHCOutOctets": "1.3.6.1.2.1.31.1.1.1.10",
	"ifHCOutUcastPkts": "1.3.6.1.2.1.31.1.1.1.11", "ifHighSpeed": "1.3.6.1.2.1.31.1.1.1.15",
	"ifAlias": "1.3.6.1.2.1.31.1.1.1.18",

	// IP-MIB, TCP-MIB, UDP-MIB, SNMP counters
	"ip": "1.3.6.1.2.1.4", "ipForwarding": "1.3.6.1.2.1.4.1", "ipDefaultTTL": "1.3.6.1.2.1.4.2",
	"ipInReceives": "1.3.6.1.2.1.4.3", "ipAddrTable": "1.3.6.1.2.1.4.20", "ipAdEntAddr": "1.3.6.1.2.1.4.20.1.1",
	"ipAdEntIfIndex": "1.3.6.1.2.1.4.20.1.2", "ipAdEntNetMask": "1.3.6.1.2.1.4.20.1.3",
	"ipRouteTable": "1.3.6.1.2.1.4.21", "ipRouteDest": "1.3.6.1.2.1.4.21.1.1", "ipRouteNextHop": "1.3.6.1.2.1.4.21.1.7",
	"ipNetToMediaTable": "1.3.6.1.2.1.4.22", "ipNetToMediaPhysAddress": "1.3.6.1.2.1.4.22.1.2",
	"ipNetToMediaNetAddress": "1.3.6.1.2.1.4.22.1.3", "icmp": "1.3.6.1.2.1.5", "tcp": "1.3.6.1.2.1.6",
	"tcpCurrEstab": "1.3.6.1.2.1.6.9", "tcpConnTable": "1.3.6.1.2.1.6.13", "tcpConnState": "1.3.6.1.2.1.6.13.1.1",
	"udp": "1.3.6.1.2.1.7", "udpTable": "1.3.6.1.2.1.7.5", "udpLocalPort": "1.3.6.1.2.1.7.5.1.2",
	"snmp": "1.3.6.1.2.1.11", "snmpInPkts": "1.3.6.1.2.1.11.1", "snmpOutPkts": "1.3.6.1.2.1.11.2",

	// HOST-RESOURCES-MIB
	"host": "1.3.6.1.2.1.25", "hrSystem": "1.3.6.1.2.1.25.1", "hrSystemUptime": "1.3.6.1.2.1.25.1.1",
	"hrSystemDate": "1.3.6.1.2.1.25.1.2", "hrSystemNumUsers": "1.3.6.1.2.1.25.1.5",
	"hrSystemProcesses": "1.3.6.1.2.1.25.1.6", "hrSystemMaxProcesses": "1.3.6.1.2.1.25.1.7",
	"hrStorage": "1.3.6.1.2.1.25.2", "hrMemorySize": "1.3.6.1.2.1.25.2.2", "hrStorageTable": "1.3.6.1.2.1.25.2.3",
	"hrStorageIndex": "1.3.6.1.2.1.25.2.3.1.1", "hrStorageType": "1.3.6.1.2.1.25.2.3.1.2",
	"hrStorageDescr": "1.3.6.1.2.1.25.2.3.1.3", "hrStorageAllocationUnits": "1.3.6.1.2.1.25.2.3.1.4",
	"hrStorageSize": "1.3.6.1.2.1.25.2.3.1.5", "hrStorageUsed": "1.3.6.1.2.1.25.2.3.1.6",
	"hrDeviceTable": "1.3.6.1.2.1.25.3.2", "hrDeviceDescr": "1.3.6.1.2.1.25.3.2.1.3",
	"hrProcessorTable": "1.3.6.1.2.1.25.3.3", "hrProcessorLoad": "1.3.6.1.2.1.25.3.3.1.2",
	"hrSWRunTable": "1.3.6.1.2.1.25.4.2", "hrSWRunName": "1.3.6.1.2.1.25.4.2.1.2",
	"hrSWRunPath": "1.3.6.1.2.1.25.4.2.1.4", "hrSWRunParameters": "1.3.6.1.2.1.25.4.2.1.5",
	"hrSWInstalledName": "1.3.6.1.2.1.25.6.3.1.2",

	// ENTITY-MIB, LLDP-MIB
	"entPhysicalTable": "1.3.6.1.2.1.47.1.1.1", "entPhysicalDescr": "1.3.6.1.2.1.47.1.1.1.1.2",
	"entPhysicalName": "1.3.6.1.2.1.47.1.1.1.1.7", "entPhysicalHardwareRev": "1.3.6.1.2.1.47.1.1.1.1.8",
	"entPhysicalFirmwareRev": "1.3.6.1.2.1.47.1.1.1.1.9", "entPhysicalSoftwareRev": "1.3.6.1.2.1.47.1.1.1.1.10",
	"entPhysicalSerialNum": "1.3.6.1.2.1.47.1.1.1.1.11", "entPhysicalMfgName": "1.3.6.1.2.1.47.1.1.1.1.12",
	"entPhysicalModelName": "1.3.6.1.2.1.47.1.1.1.1.13",
	"lldpMIB":              "1.0.8802.1.1.2", "lldpRemTable": "1.0.8802.1.1.2.1.4.1", "lldpRemPortId": "1.0.8802.1.1.2.1.4.1.1.7",
	"lldpRemPortDesc": "1.0.8802.1.1.2.1.4.1.1.8", "lldpRemSysName": "1.0.8802.1.1.2.1.4.1.1.9",
	"lldpRemSysDesc": "1.0.8802.1.1.2.1.4.1.1.10",

	// UCD-SNMP-MIB (net-snmp agents)
	"ucdavis": "1.3.6.1.4.1.2021", "memTotalReal": "1.3.6.1.4.1.2021.4.5", "memAvailReal": "1.3.6.1.4.1.2021.4.6",
	"memTotalSwap": "1.3.6.1.4.1.2021.4.3", "memAvailSwap": "1.3.6.1.4.1.2021.4.4", "memBuffer": "1.3.6.1.4.1.2021.4.14",
	"memCached": "1.3.6.1.4.1.2021.4.15", "dskTable": "1.3.6.1.4.1.2021.9", "dskPath": "1.3.6.1.4.1.2021.9.1.2",
	"dskPercent": "1.3.6.1.4.1.2021.9.1.9", "laTable": "1.3.6.1.4.1.2021.10", "laLoad": "1.3.6.1.4.1.2021.10.1.3",
	"ssCpuUser": "1.3.6.1.4.1.2021.11.9", "ssCpuSystem": "1.3.6.1.4.1.2021.11.10", "ssCpuIdle": "1.3.6.1.4.1.2021.11.11",
}

// mibByOID is the reverse index, and mibPrefixes lists the known OIDs longest first for prefix labelling.
var (
	mibByOID    = map[string]string{}
	mibPrefixes []string
)

func init() {
	for name, oid := range mibNames {
		if prev, ok := mibByOID[oid]; !ok || len(name) < len(prev) {
			mibByOID[oid] = name
		}
	}
	for oid := range mibByOID {
		if strings.Count(oid, ".") >= 4 { // label from mgmt / private / snmpV2 depth on, not "iso.3.6…"
			mibPrefixes = append(mibPrefixes, oid)
		}
	}
	sort.Slice(mibPrefixes, func(i, j int) bool { return len(mibPrefixes[i]) > len(mibPrefixes[j]) })
}

// resolveOIDName turns "sysDescr.0", "SNMPv2-MIB::sysDescr.0" or "ifTable" into a numeric OID; numeric input is
// returned as is. ok is false for an unknown name.
func resolveOIDName(s string) (string, bool) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "."))
	if s == "" {
		return "", false
	}
	if i := strings.Index(s, "::"); i >= 0 {
		s = s[i+2:]
	}
	if isNumericOID(s) {
		return s, true
	}
	name, suffix, _ := strings.Cut(s, ".")
	base, ok := mibNames[name]
	if !ok {
		for n, o := range mibNames { // case-insensitive fallback
			if strings.EqualFold(n, name) {
				base, ok = o, true
				break
			}
		}
	}
	if !ok || suffix != "" && !isNumericOID(suffix) {
		return "", false
	}
	if suffix != "" {
		return base + "." + suffix, true
	}
	return base, true
}

func isNumericOID(s string) bool {
	if s == "" {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// oidLabel names a numeric OID by its longest known prefix: 1.3.6.1.2.1.2.2.1.2.3 → "ifDescr.3".
func oidLabel(oid string) string {
	oid = strings.TrimPrefix(oid, ".")
	for _, p := range mibPrefixes {
		if oid == p {
			return mibByOID[p]
		}
		if strings.HasPrefix(oid, p+".") {
			return mibByOID[p] + oid[len(p):]
		}
	}
	return ""
}
