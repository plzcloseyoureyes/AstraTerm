/*
 * Markdown for assistant answers: GFM, no raw HTML, images never loaded (a prompt-injected image URL could leak
 * data), links open in a new tab without referrer. Code blocks get Copy / Insert / Run (shell languages) actions.
 */
import { memo, useState, type ReactNode } from 'react'
import Markdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { Check, Copy, CornerDownLeft, FileCode2, Play } from 'lucide-react'
import { toast } from 'sonner'
import { runCommand } from '@/app/commands'
import { commands } from '@/app/registry'
import { Tooltip } from '@/components/ui/tooltip'
import { cn, copyText } from '@/lib/utils'
import { insertCommand, requestRun, type RunTarget } from './run'

const SHELL_LANGS = new Set(['', 'sh', 'bash', 'shell', 'zsh', 'fish', 'ksh', 'console', 'shellsession', 'terminal', 'powershell', 'ps1', 'ps', 'pwsh', 'cmd', 'bat', 'batch', 'dos'])

const answerClass = cn(
  'text-base leading-relaxed break-words',
  '[&_h1]:mt-3 [&_h1]:mb-1 [&_h1]:text-md [&_h1]:font-semibold [&_h2]:mt-3 [&_h2]:mb-1 [&_h2]:text-md [&_h2]:font-semibold',
  '[&_h3]:mt-2 [&_h3]:mb-1 [&_h3]:font-semibold [&_p]:my-1.5 [&_ul]:my-1.5 [&_ul]:list-disc [&_ul]:pl-5 [&_ol]:my-1.5 [&_ol]:list-decimal [&_ol]:pl-5',
  '[&_li]:my-0.5 [&_a]:text-primary [&_a]:underline [&_a]:underline-offset-2 [&_hr]:my-3 [&_hr]:border-border [&_strong]:font-semibold',
  '[&_:not(pre)>code]:rounded-sm [&_:not(pre)>code]:bg-muted [&_:not(pre)>code]:px-1 [&_:not(pre)>code]:py-px [&_:not(pre)>code]:font-mono [&_:not(pre)>code]:text-[0.92em]',
  '[&_blockquote]:my-2 [&_blockquote]:border-l-2 [&_blockquote]:pl-3 [&_blockquote]:text-muted-foreground',
  '[&_table]:my-2 [&_table]:w-full [&_table]:border-collapse [&_table]:text-sm [&_td]:border [&_td]:px-2 [&_td]:py-1 [&_th]:border [&_th]:bg-muted/50 [&_th]:px-2 [&_th]:py-1 [&_th]:text-left',
  '[&>*:first-child]:mt-0 [&>*:last-child]:mb-0',
)

function textOf(node: ReactNode): string {
  if (node == null || typeof node === 'boolean') return ''
  if (typeof node === 'string' || typeof node === 'number') return String(node)
  if (Array.isArray(node)) return node.map(textOf).join('')
  if (typeof node === 'object' && 'props' in node) return textOf((node as { props: { children?: ReactNode } }).props.children)
  return ''
}

function ActionButton({ label, icon: Icon, onClick, primary }: { label: string; icon: typeof Copy; onClick: () => void; primary?: boolean }) {
  return (
    <Tooltip content={label} side="top">
      <button
          type="button"
          onClick={onClick}
          className={cn(
            'inline-flex h-6 items-center gap-1 rounded-sm px-1.5 text-xs font-medium text-muted-foreground transition-colors duration-150',
            'hover:bg-accent hover:text-accent-foreground focus-visible:ring-2 focus-visible:ring-ring/60 focus-visible:outline-none',
            primary && 'text-foreground/85',
          )}
        >
          <Icon className="size-3.5" />
        <span className="hidden @[22rem]:inline">{label}</span>
      </button>
    </Tooltip>
  )
}

function CodeBlock({ code, lang, target, streaming }: { code: string; lang: string; target?: RunTarget; streaming?: boolean }) {
  const [copied, setCopied] = useState(false)
  const shell = SHELL_LANGS.has(lang.toLowerCase())
  const text = code.replace(/\n$/, '')
  const copy = async () => {
    if (await copyText(text)) {
      setCopied(true)
      setTimeout(() => setCopied(false), 1400)
    } else toast.error('Copy failed')
  }
  return (
    <div className="group/code my-2 overflow-hidden rounded-md border border-border/70 bg-muted/40">
      <div className="flex h-7 items-center gap-1 border-b border-border/60 bg-muted/60 pr-1 pl-2.5">
        <span className="text-2xs font-medium tracking-wide text-muted-foreground uppercase">{lang || 'shell'}</span>
        <span className="flex-1" />
        {!streaming && (
          <>
            <ActionButton label={copied ? 'Copied' : 'Copy'} icon={copied ? Check : Copy} onClick={() => void copy()} />
            {shell ? (
              <>
                <ActionButton label="Insert" icon={CornerDownLeft} onClick={() => void insertCommand(text, target)} primary />
                <ActionButton label="Run…" icon={Play} onClick={() => void requestRun(text, { target })} primary />
              </>
            ) : (
              commands.get('editor.openText') && (
                <ActionButton
                  label="Open in editor"
                  icon={FileCode2}
                  onClick={() => void runCommand('editor.openText', { title: `AI snippet.${lang || 'txt'}`, content: text, language: lang || undefined })}
                />
              )
            )}
          </>
        )}
      </div>
      <pre className="overflow-x-auto px-3 py-2 font-mono text-sm leading-relaxed">
        <code>{text}</code>
      </pre>
    </div>
  )
}

export const AnswerMarkdown = memo(function AnswerMarkdown({ text, target, streaming }: { text: string; target?: RunTarget; streaming?: boolean }) {
  return (
    <div className={answerClass}>
      <Markdown
        remarkPlugins={[remarkGfm]}
        skipHtml
        components={{
          a: ({ node: _node, ...props }) => <a {...props} target="_blank" rel="noopener noreferrer nofollow" />,
          img: ({ node: _node, alt }) => <span className="text-muted-foreground">[image{alt ? `: ${alt}` : ''}]</span>,
          pre: ({ node: _node, children }) => {
            const child = Array.isArray(children) ? children[0] : children
            const className = (child as { props?: { className?: string } })?.props?.className ?? ''
            const lang = /language-([\w+#.-]+)/.exec(className)?.[1] ?? ''
            return <CodeBlock code={textOf(children)} lang={lang} target={target} streaming={streaming} />
          },
        }}
      >
        {text}
      </Markdown>
    </div>
  )
})
