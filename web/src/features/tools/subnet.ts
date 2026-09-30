/*
 * Pure IPv4/IPv6 subnet math for the CIDR calculator (TOOL-8). No dependencies; IPv4 uses 32-bit integer ops,
 * IPv6 uses BigInt.
 */

interface SubnetResultV4 {
  family: 4
  cidr: string
  address: string
  netmask: string
  wildcard: string
  network: string
  broadcast: string
  firstHost: string
  lastHost: string
  hostCount: string
  usableHosts: string
  prefix: number
  ipClass: string
  isPrivate: boolean
}

interface SubnetResultV6 {
  family: 6
  cidr: string
  address: string
  network: string
  prefix: number
  firstAddress: string
  lastAddress: string
  addressCount: string
}

export type SubnetResult = SubnetResultV4 | SubnetResultV6

function ipv4ToInt(ip: string): number | null {
  const parts = ip.split('.')
  if (parts.length !== 4) return null
  let n = 0
  for (const p of parts) {
    if (!/^\d{1,3}$/.test(p)) return null
    const v = Number(p)
    if (v > 255) return null
    n = (n << 8) | v
  }
  return n >>> 0
}

function intToIpv4(n: number): string {
  return [(n >>> 24) & 255, (n >>> 16) & 255, (n >>> 8) & 255, n & 255].join('.')
}

function ipv4Class(n: number): string {
  const first = (n >>> 24) & 255
  if (first < 128) return 'A'
  if (first < 192) return 'B'
  if (first < 224) return 'C'
  if (first < 240) return 'D (multicast)'
  return 'E (reserved)'
}

function ipv4Private(n: number): boolean {
  const a = (n >>> 24) & 255
  const b = (n >>> 16) & 255
  return a === 10 || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168) || (a === 127) || (a === 169 && b === 254)
}

function calcV4(address: string, prefix: number): SubnetResultV4 | { error: string } {
  const ip = ipv4ToInt(address)
  if (ip === null) return { error: 'Invalid IPv4 address' }
  if (prefix < 0 || prefix > 32) return { error: 'IPv4 prefix must be 0–32' }
  const mask = prefix === 0 ? 0 : (0xffffffff << (32 - prefix)) >>> 0
  const network = (ip & mask) >>> 0
  const broadcast = (network | (~mask >>> 0)) >>> 0
  const total = Math.pow(2, 32 - prefix)
  const usable = prefix >= 31 ? total : Math.max(0, total - 2)
  const firstHost = prefix >= 31 ? network : (network + 1) >>> 0
  const lastHost = prefix >= 31 ? broadcast : (broadcast - 1) >>> 0
  return {
    family: 4,
    cidr: `${intToIpv4(network)}/${prefix}`,
    address: intToIpv4(ip),
    netmask: intToIpv4(mask),
    wildcard: intToIpv4(~mask >>> 0),
    network: intToIpv4(network),
    broadcast: intToIpv4(broadcast),
    firstHost: intToIpv4(firstHost),
    lastHost: intToIpv4(lastHost),
    hostCount: total.toLocaleString(),
    usableHosts: usable.toLocaleString(),
    prefix,
    ipClass: ipv4Class(ip),
    isPrivate: ipv4Private(ip),
  }
}

// ---- IPv6 ------------------------------------------------------------------------------------------------------------

function ipv6ToBig(ip: string): bigint | null {
  ip = ip.trim()
  // Strip a zone id.
  const pct = ip.indexOf('%')
  if (pct >= 0) ip = ip.slice(0, pct)
  if (!ip.includes(':')) return null
  let head = ip
  let embeddedV4 = ''
  // IPv4-mapped tail (e.g. ::ffff:1.2.3.4)
  const lastColon = ip.lastIndexOf(':')
  const tail = ip.slice(lastColon + 1)
  if (tail.includes('.')) {
    const v4 = ipv4ToInt(tail)
    if (v4 === null) return null
    embeddedV4 = ((v4 >>> 16) & 0xffff).toString(16) + ':' + (v4 & 0xffff).toString(16)
    head = ip.slice(0, lastColon + 1) + embeddedV4
  }
  const dbl = head.split('::')
  if (dbl.length > 2) return null
  const expand = (s: string) => (s === '' ? [] : s.split(':'))
  let groups: string[]
  if (dbl.length === 2) {
    const left = expand(dbl[0])
    const right = expand(dbl[1])
    const fill = 8 - left.length - right.length
    if (fill < 0) return null
    groups = [...left, ...Array(fill).fill('0'), ...right]
  } else {
    groups = expand(head)
  }
  if (groups.length !== 8) return null
  let n = 0n
  for (const g of groups) {
    if (!/^[0-9a-fA-F]{1,4}$/.test(g)) return null
    n = (n << 16n) | BigInt(parseInt(g, 16))
  }
  return n
}

function bigToIpv6(n: bigint): string {
  const groups: string[] = []
  for (let i = 7; i >= 0; i--) {
    groups.push(((n >> BigInt(i * 16)) & 0xffffn).toString(16))
  }
  // Compress the longest run of zero groups.
  let bestStart = -1
  let bestLen = 0
  let curStart = -1
  let curLen = 0
  for (let i = 0; i < 8; i++) {
    if (groups[i] === '0') {
      if (curStart < 0) curStart = i
      curLen++
      if (curLen > bestLen) {
        bestLen = curLen
        bestStart = curStart
      }
    } else {
      curStart = -1
      curLen = 0
    }
  }
  if (bestLen > 1) {
    const before = groups.slice(0, bestStart).join(':')
    const after = groups.slice(bestStart + bestLen).join(':')
    return `${before}::${after}`
  }
  return groups.join(':')
}

function calcV6(address: string, prefix: number): SubnetResultV6 | { error: string } {
  const ip = ipv6ToBig(address)
  if (ip === null) return { error: 'Invalid IPv6 address' }
  if (prefix < 0 || prefix > 128) return { error: 'IPv6 prefix must be 0–128' }
  const hostBits = BigInt(128 - prefix)
  const mask = hostBits === 128n ? 0n : ((1n << 128n) - 1n) ^ ((1n << hostBits) - 1n)
  const network = ip & mask
  const last = network | ((1n << hostBits) - 1n)
  const count = 1n << hostBits
  return {
    family: 6,
    cidr: `${bigToIpv6(network)}/${prefix}`,
    address: bigToIpv6(ip),
    network: bigToIpv6(network),
    prefix,
    firstAddress: bigToIpv6(network),
    lastAddress: bigToIpv6(last),
    addressCount: count.toString(),
  }
}

/** Parse and compute a subnet from "address/prefix" (IPv4 or IPv6). */
export function calcSubnet(input: string): SubnetResult | { error: string } {
  const trimmed = input.trim()
  if (!trimmed) return { error: '' }
  const [addr, prefixStr] = trimmed.split('/')
  const isV6 = addr.includes(':')
  const defPrefix = isV6 ? 64 : 32
  const prefix = prefixStr !== undefined ? Number(prefixStr) : defPrefix
  if (prefixStr !== undefined && (!/^\d+$/.test(prefixStr) || Number.isNaN(prefix))) return { error: 'Invalid prefix' }
  return isV6 ? calcV6(addr, prefix) : calcV4(addr, prefix)
}
