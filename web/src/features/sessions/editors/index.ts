/*
 * Registers every protocol editor (side effects). Order of the protocol picker comes from each spec's `order`:
 * terminal (SSH … IPMI), graphical (RDP, VNC), files (SFTP, FTP, S3).
 */
import './ssh'
import './telnet'
import './rlogin'
import './raw'
import './serial'
import './local'
import './mosh'
import './docker'
import './kube'
import './winrm'
import './ipmi'
import './rdp'
import './vnc'
import './sftp'
import './ftp'
import './s3'

export { getProtocolProfile, getProtocolSpec, isSshFamily, type ProtocolProfile, type ProtocolSpec } from './define'
