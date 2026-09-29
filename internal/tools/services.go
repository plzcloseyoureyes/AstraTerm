package tools

// tcpServices maps common TCP ports to their IANA service name (RESEARCH TOOL-5 "embedded services map"). It covers
// the ports a sysadmin scans for; unknown ports simply have no name. Kept intentionally compact (single binary).
var tcpServices = map[int]string{
	1: "tcpmux", 7: "echo", 9: "discard", 13: "daytime", 17: "qotd", 19: "chargen", 20: "ftp-data", 21: "ftp",
	22: "ssh", 23: "telnet", 25: "smtp", 37: "time", 43: "whois", 49: "tacacs", 53: "domain", 70: "gopher",
	79: "finger", 80: "http", 81: "http-alt", 88: "kerberos", 102: "iso-tsap", 110: "pop3", 111: "rpcbind",
	113: "ident", 119: "nntp", 123: "ntp", 135: "msrpc", 137: "netbios-ns", 138: "netbios-dgm", 139: "netbios-ssn",
	143: "imap", 161: "snmp", 162: "snmptrap", 179: "bgp", 199: "smux", 389: "ldap", 427: "svrloc", 443: "https",
	444: "snpp", 445: "microsoft-ds", 465: "smtps", 500: "isakmp", 502: "modbus", 512: "exec", 513: "login",
	514: "shell", 515: "printer", 520: "route", 523: "ibm-db2", 540: "uucp", 543: "klogin", 544: "kshell",
	548: "afp", 554: "rtsp", 587: "submission", 623: "ipmi-rmcp", 631: "ipp", 636: "ldaps", 646: "ldp",
	873: "rsync", 902: "vmware-auth", 989: "ftps-data", 990: "ftps", 993: "imaps", 995: "pop3s",
	1080: "socks", 1099: "rmiregistry", 1194: "openvpn", 1214: "kazaa", 1241: "nessus", 1311: "dell-openmanage",
	1352: "lotusnotes", 1433: "ms-sql-s", 1434: "ms-sql-m", 1494: "citrix-ica", 1521: "oracle", 1604: "citrix",
	1701: "l2tp", 1723: "pptp", 1725: "steam", 1755: "wms", 1812: "radius", 1813: "radius-acct", 1883: "mqtt",
	1900: "upnp", 2000: "cisco-sccp", 2049: "nfs", 2082: "cpanel", 2083: "cpanel-ssl", 2086: "whm", 2087: "whm-ssl",
	2095: "webmail", 2096: "webmail-ssl", 2181: "zookeeper", 2222: "ssh-alt", 2375: "docker", 2376: "docker-tls",
	2379: "etcd-client", 2380: "etcd-server", 2483: "oracle-db", 2484: "oracle-db-ssl", 2601: "zebra",
	3000: "ppp", 3128: "squid-http", 3260: "iscsi", 3268: "globalcat-ldap", 3269: "globalcat-ldaps",
	3283: "net-assistant", 3306: "mysql", 3333: "dec-notes", 3389: "ms-wbt-server", 3478: "stun", 3527: "beserver",
	3632: "distcc", 3689: "daap", 3690: "svn", 3724: "blizzard", 4000: "remoteanything", 4022: "dnox",
	4040: "yo-main", 4123: "openrsm", 4369: "epmd", 4433: "https-alt", 4500: "ipsec-nat", 4567: "tram",
	4711: "e-lms", 4786: "smart-install", 4822: "guacd", 4899: "radmin", 5000: "upnp-http", 5001: "commplex",
	5009: "airport-admin", 5060: "sip", 5061: "sip-tls", 5222: "xmpp-client", 5269: "xmpp-server",
	5353: "mdns", 5432: "postgresql", 5555: "freeciv", 5601: "kibana", 5666: "nrpe", 5672: "amqp",
	5683: "coap", 5800: "vnc-http", 5900: "vnc", 5901: "vnc-1", 5902: "vnc-2", 5938: "teamviewer",
	5984: "couchdb", 5985: "winrm-http", 5986: "winrm-https", 6000: "x11", 6001: "x11-1", 6379: "redis",
	6443: "kubernetes-api", 6514: "syslog-tls", 6660: "irc", 6666: "irc", 6667: "irc", 6697: "ircs-u",
	7000: "afs3", 7001: "weblogic", 7070: "realserver", 7077: "spark", 7443: "oracle-https", 7474: "neo4j",
	7687: "bolt", 8000: "http-alt", 8008: "http", 8009: "ajp13", 8020: "hadoop", 8080: "http-proxy",
	8081: "http-alt", 8086: "influxdb", 8088: "radan-http", 8089: "splunkd", 8123: "clickhouse-http",
	8140: "puppet", 8161: "activemq", 8200: "vault", 8222: "vmware-vsphere", 8291: "mikrotik-winbox",
	8333: "bitcoin", 8384: "syncthing", 8443: "https-alt", 8500: "consul", 8501: "consul-tls", 8530: "wsus",
	8600: "consul-dns", 8686: "sun-as-jmxrmi", 8728: "mikrotik-api", 8765: "ultraseek-http", 8834: "nessus-ui",
	8888: "http-alt", 8983: "solr", 9000: "cslistener", 9001: "tor-orport", 9042: "cassandra", 9080: "glrpc",
	9090: "prometheus", 9092: "kafka", 9100: "jetdirect", 9200: "elasticsearch", 9300: "elasticsearch-node",
	9418: "git", 9443: "https-alt", 9999: "abyss", 10000: "webmin", 10250: "kubelet", 10255: "kubelet-ro",
	11211: "memcached", 15672: "rabbitmq-mgmt", 16992: "amt-soap-http", 16993: "amt-soap-https",
	17500: "dropbox-lansync", 19999: "dnp", 20000: "dnp3", 25565: "minecraft", 27015: "steam-game",
	27017: "mongodb", 27018: "mongodb-shard", 32400: "plex", 33060: "mysqlx", 49152: "upnp-dynamic",
	50000: "sap", 50070: "hadoop-namenode", 54321: "unassigned",
}

// udpServices maps common UDP ports to their service name.
var udpServices = map[int]string{
	53: "domain", 67: "dhcps", 68: "dhcpc", 69: "tftp", 111: "rpcbind", 123: "ntp", 137: "netbios-ns",
	138: "netbios-dgm", 161: "snmp", 162: "snmptrap", 179: "bgp", 500: "isakmp", 514: "syslog", 520: "route",
	623: "ipmi-rmcp", 1194: "openvpn", 1434: "ms-sql-m", 1701: "l2tp", 1812: "radius", 1813: "radius-acct",
	1900: "upnp", 2049: "nfs", 3478: "stun", 4500: "ipsec-nat", 5060: "sip", 5353: "mdns", 5683: "coap",
	6514: "syslog-tls", 11211: "memcached",
}

// serviceName returns the well-known service name for a port, or "".
func serviceName(port int, udp bool) string {
	if udp {
		return udpServices[port]
	}
	return tcpServices[port]
}

// topPorts is the "top 100"-ish set of the most commonly open TCP ports (Nmap-style, condensed) used by the
// `top100` / `common` port specs and the network scanner.
var topPorts = []int{
	7, 20, 21, 22, 23, 25, 26, 37, 53, 79, 80, 81, 88, 106, 110, 111, 113, 119, 135, 139, 143, 144, 179, 199,
	389, 427, 443, 444, 445, 465, 513, 514, 515, 543, 544, 548, 554, 587, 631, 646, 873, 990, 993, 995,
	1025, 1026, 1027, 1028, 1029, 1080, 1110, 1194, 1433, 1521, 1723, 1755, 1900, 2000, 2001, 2049, 2121,
	2181, 2222, 2375, 2376, 2379, 3000, 3128, 3260, 3306, 3389, 3690, 4444, 4786, 4822, 5000, 5060, 5222,
	5432, 5555, 5601, 5672, 5900, 5901, 5985, 5986, 6000, 6379, 6443, 6667, 7001, 8000, 8008, 8009, 8080,
	8081, 8443, 8500, 8888, 9000, 9090, 9100, 9200, 9300, 9418, 10000, 11211, 27017, 32400, 49152,
}
