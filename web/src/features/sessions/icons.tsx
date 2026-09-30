/*
 * Session / folder icons (SM-1): a curated built-in set stored as `lucide:<Name>`, or a small custom image stored as a
 * `data:image/...;base64,` URL (≤ 64 KiB, travels with exports). Custom images are only ever rendered through <img>,
 * so an uploaded SVG cannot run script; any other value (e.g. a remote URL on a shared item) is ignored.
 */
import type { CSSProperties } from 'react'
import {
  Anchor,
  Antenna,
  Atom,
  Beer,
  Bird,
  Book,
  Bookmark,
  Bot,
  Box,
  Boxes,
  Brain,
  Briefcase,
  Bug,
  Building,
  Cable,
  Camera,
  Car,
  Cat,
  CircuitBoard,
  Cloud,
  CloudCog,
  Code,
  Coffee,
  Cog,
  Compass,
  Computer,
  Container,
  Cpu,
  Crown,
  Database,
  Dog,
  Earth,
  Factory,
  Fish,
  Flag,
  Flame,
  FlaskConical,
  Folder,
  FolderOpen,
  Gamepad2,
  Gem,
  GitBranch,
  Globe,
  GraduationCap,
  HardDrive,
  Heart,
  House,
  KeyRound,
  Laptop,
  Layers,
  Leaf,
  Lock,
  Map as MapIcon,
  MapPin,
  MemoryStick,
  Microchip,
  Microscope,
  Monitor,
  Moon,
  Mountain,
  Music,
  Network,
  Orbit,
  Package,
  PcCase,
  Plane,
  Plug,
  Printer,
  Rabbit,
  Radar,
  Radio,
  Rocket,
  Router,
  Satellite,
  Server,
  ServerCog,
  Shell,
  Shield,
  ShieldCheck,
  Ship,
  Skull,
  Smartphone,
  Snowflake,
  Sprout,
  SquareTerminal,
  Star,
  Store,
  Sun,
  Tablet,
  Target,
  Telescope,
  Terminal,
  TreePine,
  Trophy,
  Truck,
  Turtle,
  Tv,
  User,
  Users,
  Usb,
  Warehouse,
  Webhook,
  Wifi,
  Wrench,
  Zap,
  type LucideIcon,
} from 'lucide-react'
import type { IconType } from '@/app/registry'
import { protocolIcon } from '@/app/protocols'
import { cn } from '@/lib/utils'

/** Built-in icon set, grouped for the picker. Keys are the stored names (`lucide:<key>`). */
export const ICON_GROUPS: { label: string; icons: Record<string, LucideIcon> }[] = [
  {
    label: 'Machines',
    icons: { Server, ServerCog, Database, HardDrive, Cpu, MemoryStick, CircuitBoard, Microchip, PcCase, Computer, Monitor, Laptop, Tablet, Smartphone, Tv, Printer, Camera },
  },
  {
    label: 'Network',
    icons: { Router, Network, Wifi, Antenna, Radio, Satellite, Cable, Plug, Usb, Globe, Earth, Cloud, CloudCog, Webhook, Radar },
  },
  {
    label: 'Software',
    icons: { Terminal, SquareTerminal, Shell, Code, GitBranch, Container, Box, Boxes, Package, Layers, Bug, FlaskConical, Rocket, Bot, Cog, Wrench },
  },
  {
    label: 'Security',
    icons: { Shield, ShieldCheck, Lock, KeyRound },
  },
  {
    label: 'Places & people',
    icons: { House, Building, Factory, Warehouse, Store, Briefcase, GraduationCap, User, Users, MapPin, Map: MapIcon, Compass, Plane, Car, Ship, Truck },
  },
  {
    label: 'Symbols',
    icons: {
      Star,
      Heart,
      Flame,
      Zap,
      Leaf,
      Sprout,
      TreePine,
      Mountain,
      Snowflake,
      Sun,
      Moon,
      Anchor,
      Flag,
      Bookmark,
      Target,
      Trophy,
      Crown,
      Gem,
      Gamepad2,
      Music,
      Book,
      Coffee,
      Beer,
      Brain,
      Atom,
      Microscope,
      Telescope,
      Orbit,
      Cat,
      Dog,
      Bird,
      Fish,
      Rabbit,
      Turtle,
      Skull,
    },
  },
]

const CATALOG: Record<string, LucideIcon> = Object.assign({}, ...ICON_GROUPS.map((g) => g.icons))

export const LUCIDE_PREFIX = 'lucide:'
const DATA_URL_RE = /^data:image\/(?:png|jpeg|gif|webp|bmp|svg\+xml|x-icon|vnd\.microsoft\.icon);base64,[A-Za-z0-9+/]+={0,2}$/
/** Backend limit for connection/folder `icon` (64 KiB). */
export const MAX_ICON_CHARS = 64 * 1024

export function isCustomImageIcon(icon: string | undefined | null): icon is string {
  return !!icon && icon.length <= MAX_ICON_CHARS && icon.startsWith('data:image/') && DATA_URL_RE.test(icon)
}

/** Built-in icon component for a stored value, if it is one. */
function builtinIcon(icon: string | undefined | null): IconType | undefined {
  if (!icon || !icon.startsWith(LUCIDE_PREFIX)) return undefined
  return CATALOG[icon.slice(LUCIDE_PREFIX.length)]
}

/** Renders a stored icon value (built-in or custom image) or the fallback. */
export function EntityIcon({
  icon,
  fallback,
  color,
  className,
  style,
}: {
  icon?: string | null
  fallback: IconType
  color?: string | null
  className?: string
  style?: CSSProperties
}) {
  if (isCustomImageIcon(icon)) {
    return <img src={icon} alt="" aria-hidden draggable={false} className={cn('size-4 shrink-0 object-contain', className)} style={style} />
  }
  const Icon = builtinIcon(icon) ?? fallback
  const el = <Icon aria-hidden className={cn('size-4 shrink-0', className)} />
  const c = safeColor(color)
  // Icons draw with currentColor: colour them through a box-less wrapper (works for any IconType).
  return c || style ? (
    <span className="contents" style={{ ...style, ...(c ? { color: c } : {}) }}>
      {el}
    </span>
  ) : (
    el
  )
}

/** Icon of a connection: custom icon, else the protocol icon. */
export function ConnectionIcon({
  protocol,
  icon,
  color,
  className,
}: {
  protocol: string
  icon?: string | null
  color?: string | null
  className?: string
}) {
  return <EntityIcon icon={icon} fallback={protocolIcon(protocol)} color={color} className={className} />
}

/** Icon of a folder (open/closed variants unless a custom icon is set). */
export function FolderIcon({ icon, color, open, className }: { icon?: string | null; color?: string | null; open?: boolean; className?: string }) {
  return <EntityIcon icon={icon} fallback={open ? FolderOpen : Folder} color={color} className={className} />
}

// ---------------------------------------------------------------------------------------------------------------------
// Colours
// ---------------------------------------------------------------------------------------------------------------------

/** Named colours offered in menus (same palette as the swatch picker). */
export const NAMED_COLORS: { name: string; value: string }[] = [
  { name: 'Red', value: '#ef4444' },
  { name: 'Orange', value: '#f97316' },
  { name: 'Amber', value: '#f59e0b' },
  { name: 'Yellow', value: '#eab308' },
  { name: 'Lime', value: '#84cc16' },
  { name: 'Green', value: '#22c55e' },
  { name: 'Teal', value: '#14b8a6' },
  { name: 'Cyan', value: '#06b6d4' },
  { name: 'Sky', value: '#0ea5e9' },
  { name: 'Blue', value: '#3b82f6' },
  { name: 'Indigo', value: '#6366f1' },
  { name: 'Violet', value: '#8b5cf6' },
  { name: 'Purple', value: '#a855f7' },
  { name: 'Pink', value: '#ec4899' },
  { name: 'Rose', value: '#f43f5e' },
  { name: 'Slate', value: '#64748b' },
]

/** Accept only plain CSS colours we generate (hex / rgb / hsl / oklch / named letters) — never url() or expressions. */
export function safeColor(color: string | undefined | null): string | undefined {
  if (!color) return undefined
  const c = color.trim()
  if (/^#[0-9a-f]{3,8}$/i.test(c)) return c
  if (/^(?:rgb|rgba|hsl|hsla|oklch|oklab)\([0-9.,%\s/+-]+\)$/i.test(c)) return c
  if (/^[a-z]{3,20}$/i.test(c)) return c
  return undefined
}

const dotCache = new Map<string, IconType>()

/** A tiny colour-dot "icon" for menu items. */
export function colorDotIcon(color: string): IconType {
  let icon = dotCache.get(color)
  if (!icon) {
    const Dot = ({ className }: { className?: string }) => (
      <span aria-hidden className={cn('inline-block size-3 shrink-0 rounded-full border border-black/10', className)} style={{ background: color }} />
    )
    icon = Dot
    dotCache.set(color, icon)
  }
  return icon
}
