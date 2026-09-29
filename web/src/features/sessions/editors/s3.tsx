/*
 * S3 / S3-compatible storage editor (PROTO-26; SPEC §5.3 "s3"): AWS or a custom endpoint (MinIO, R2, Wasabi, B2).
 * The secret access key is a write-only secret (`secretAccessKey`).
 */
import { Cloud } from 'lucide-react'
import type { ProtocolEditorProps } from '@/app/registry'
import type { Connection } from '@/api/types'
import { defineProtocol, type ValidationErrors } from './define'
import { ComboOption, OptionSection, SecretOption, SwitchOption, TextOption, optString, type ComboSuggestion } from './fields'

const AWS_REGIONS: ComboSuggestion[] = [
  'us-east-1',
  'us-east-2',
  'us-west-1',
  'us-west-2',
  'ca-central-1',
  'sa-east-1',
  'eu-west-1',
  'eu-west-2',
  'eu-west-3',
  'eu-central-1',
  'eu-central-2',
  'eu-north-1',
  'eu-south-1',
  'me-central-1',
  'af-south-1',
  'ap-south-1',
  'ap-southeast-1',
  'ap-southeast-2',
  'ap-northeast-1',
  'ap-northeast-2',
  'ap-northeast-3',
  'ap-east-1',
  'auto',
].map((value) => ({ value, description: value === 'auto' ? 'Cloudflare R2' : undefined }))

export function S3Editor({ value, onChange }: ProtocolEditorProps) {
  const p = { value, onChange }
  return (
    <div className="grid gap-5">
      <OptionSection title="Service">
        <TextOption
          {...p}
          name="endpoint"
          label="Endpoint"
          placeholder="Amazon S3"
          mono
          trim
          hint="Leave empty for AWS; e.g. https://minio.example.com:9000 for compatible stores."
          className="@lg:col-span-2"
        />
        <ComboOption {...p} name="region" label="Region" mono placeholder="us-east-1" suggestions={AWS_REGIONS} />
        <div className="pt-6">
          <SwitchOption {...p} name="pathStyle" label="Path-style addressing" hint="Needed by MinIO, Ceph and most self-hosted stores." />
        </div>
      </OptionSection>
      <OptionSection title="Credentials">
        <TextOption {...p} name="accessKeyId" label="Access key ID" placeholder="Server credentials (environment / IAM role)" mono trim />
        <SecretOption {...p} secret="secretAccessKey" label="Secret access key" placeholder="Not stored" />
      </OptionSection>
      <OptionSection title="Browser">
        <TextOption {...p} name="bucket" label="Bucket" placeholder="All buckets" mono trim />
        <TextOption {...p} name="initialPath" label="Initial prefix" placeholder="/" mono />
      </OptionSection>
    </div>
  )
}

function validateS3(c: Connection): ValidationErrors {
  const errors: ValidationErrors = {}
  const endpoint = optString(c, 'endpoint').trim()
  if (endpoint) {
    try {
      const u = new URL(endpoint)
      if (u.protocol !== 'https:' && u.protocol !== 'http:') throw new Error('scheme')
    } catch {
      errors['options.endpoint'] = 'Enter an http:// or https:// URL'
    }
  }
  const bucket = optString(c, 'bucket').trim()
  if (bucket && !/^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/.test(bucket)) errors['options.bucket'] = 'Bucket names use 3–63 lowercase letters, digits, dots and dashes'
  if (optString(c, 'accessKeyId').trim() && !(c.secretKeys ?? []).includes('secretAccessKey') && !c.secrets?.secretAccessKey) {
    errors['secrets.secretAccessKey'] = 'Enter the secret access key for this access key ID'
  }
  return errors
}

defineProtocol({
  protocol: 's3',
  label: 'S3',
  icon: Cloud,
  defaultPort: 443,
  group: 'files',
  order: 320,
  description: 'Amazon S3 and compatible object storage',
  component: S3Editor,
  profile: { host: 'hidden', port: false, username: false, auth: 'none', kind: 'files', network: true },
  validate: validateS3,
})
