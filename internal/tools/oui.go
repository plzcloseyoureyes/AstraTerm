package tools

import "strings"

// ouiVendors maps a 24-bit OUI prefix (uppercase hex, no separators) to a vendor name. This is a curated subset of the
// IEEE registry covering the prefixes seen most on real networks (RESEARCH TOOL-4 "embedded OUI database for common
// prefixes") — a full 30k-entry database would bloat the single binary; unknown prefixes simply return "".
var ouiVendors = map[string]string{
	"000569": "VMware", "000C29": "VMware", "001C14": "VMware", "005056": "VMware", "000F4B": "Oracle VirtualBox",
	"080027": "VirtualBox", "0A0027": "VirtualBox", "525400": "QEMU/KVM", "001C42": "Parallels", "00163E": "Xen",
	"00155D": "Microsoft Hyper-V", "0017FA": "Microsoft", "F01FAF": "Dell", "00188B": "Dell", "001EC9": "Dell",
	"B8CA3A": "Dell", "18DBF2": "Dell", "D067E5": "Dell", "001AA0": "Dell", "F8BC12": "Dell", "00219B": "Dell",
	"0025B3": "HP", "001B78": "HP", "002481": "HP", "3863BB": "HP", "9457A5": "HP", "70106F": "HP", "A0481C": "HP",
	"001A4B": "HP", "001CC4": "HP", "0026B9": "Dell", "000D9D": "HP", "001321": "HP",
	"001B21": "Intel", "001E67": "Intel", "0026C7": "Intel", "3C970E": "Intel", "A0369F": "Intel", "001517": "Intel",
	"00A0C9": "Intel", "8086F2": "Intel", "B4969F": "Intel", "001111": "Intel",
	"0003BA": "Sun/Oracle", "080020": "Sun/Oracle", "00144F": "Oracle",
	"000C42": "Routerboard/MikroTik", "4C5E0C": "MikroTik", "6C3B6B": "MikroTik", "E48D8C": "MikroTik",
	"CC2DE0": "MikroTik", "18FD74": "MikroTik", "D4CA6D": "MikroTik", "744D28": "MikroTik", "48A98A": "MikroTik",
	"0004F2": "Polycom", "000B82": "Grandstream", "000E08": "Cisco/Linksys", "001839": "Cisco-Linksys",
	"00040B": "Cisco", "000142": "Cisco", "00095B": "Netgear", "0018E7": "Cameo/Netgear", "204E7F": "Netgear",
	"A00460": "Netgear", "C03F0E": "Netgear", "9C3DCF": "Netgear", "3C3786": "Netgear",
	"001CDF": "Belkin", "944452": "Belkin", "EC1A59": "Belkin",
	"0018F8": "Cisco-Linksys", "58BF25": "Xiaomi", "286C07": "Xiaomi", "64B473": "Xiaomi", "F8A45F": "Xiaomi",
	"F0272D": "Xiaomi", "50EC50": "Xiaomi", "78115B": "Espressif", "240AC4": "Espressif", "3C6105": "Espressif",
	"7CDFA1": "Espressif", "84F3EB": "Espressif", "A020A6": "Espressif", "ECFABC": "Espressif", "BCDDC2": "Espressif",
	"B827EB": "Raspberry Pi", "DCA632": "Raspberry Pi", "E45F01": "Raspberry Pi", "28CDC1": "Raspberry Pi",
	"D83ADD": "Raspberry Pi", "2CCF67": "Raspberry Pi",
	"F0D5BF": "Apple", "3C0754": "Apple", "A8667F": "Apple", "F0DBF8": "Apple", "F81EDF": "Apple", "D0817A": "Apple",
	"AC87A3": "Apple", "8863DF": "Apple", "60FDA6": "Apple", "F0189E": "Apple", "94E96A": "Apple", "6C4008": "Apple",
	"000D93": "Apple", "0016CB": "Apple", "001B63": "Apple", "0017F2": "Apple", "3451C9": "Apple",
	"001DD8": "Microsoft", "7C1E52": "Microsoft", "0050F2": "Microsoft", "C83F26": "Microsoft",
	"FCFBFB": "Cisco", "00000C": "Cisco", "000163": "Cisco", "0007EB": "Cisco", "001BD4": "Cisco", "E8B748": "Cisco",
	"00259C": "Cisco", "6C200D": "Cisco", "F866F2": "Cisco", "88F031": "Cisco", "70DB98": "Cisco",
	"001560": "HP", "000BDB": "Dell",
	"0090A9": "Western Digital", "00146C": "Netgear", "20CF30": "ASUSTek", "1C872C": "ASUSTek", "2C56DC": "ASUSTek",
	"AC220B": "ASUSTek", "50465D": "ASUSTek", "04D4C4": "ASUSTek", "708BCD": "ASUSTek",
	"D8CB8A": "Micro-Star", "4CCC6A": "Micro-Star", "00D861": "Micro-Star",
	"001E8C": "ASUSTek", "F46D04": "ASUSTek", "38D547": "ASUSTek",
	"000E0C": "Intel", "0013CE": "Intel",
	"D4AE52": "Dell", "782BCB": "Dell", "5CF9DD": "Dell", "0CC47A": "Super Micro", "003048": "Super Micro",
	"AC1F6B": "Super Micro", "3CECEF": "Super Micro",
	"001C23": "Dell", "18A905": "HP", "FC15B4": "HP", "10604B": "HP",
	"E4A7A0": "Intel", "00E04C": "Realtek", "52540A": "Realtek", "001346": "D-Link", "1CBDB9": "D-Link",
	"C8BE19": "D-Link", "340804": "D-Link", "784476": "TP-Link", "50C7BF": "TP-Link", "EC086B": "TP-Link",
	"A42BB0": "TP-Link", "C46E1F": "TP-Link", "6466B3": "TP-Link", "9C5322": "TP-Link", "003192": "Huawei",
	"48AD08": "Huawei", "781DBA": "Huawei", "E0247F": "Huawei", "04BD70": "Huawei", "10C61F": "Samsung",
	"BC7654": "Samsung", "8425DB": "Samsung", "5C0A5B": "Samsung", "F409D8": "Samsung",
}

// vendorForMAC returns the vendor for a MAC address (any common separator style), or "".
func vendorForMAC(mac string) string {
	hex := strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9', r >= 'A' && r <= 'F':
			return r
		case r >= 'a' && r <= 'f':
			return r - 32
		default:
			return -1
		}
	}, mac)
	if len(hex) < 6 {
		return ""
	}
	return ouiVendors[hex[:6]]
}
