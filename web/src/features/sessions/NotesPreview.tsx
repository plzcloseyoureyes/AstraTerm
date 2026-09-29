/*
 * Markdown rendering of session notes (SM-4). Lazy-loaded; raw HTML is not rendered (react-markdown default) and
 * links open in a new tab without referrer.
 */
import Markdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { cn } from '@/lib/utils'

export const markdownClass = cn(
  'text-base leading-relaxed break-words',
  '[&_h1]:mt-2 [&_h1]:mb-1 [&_h1]:text-lg [&_h1]:font-semibold [&_h2]:mt-2 [&_h2]:mb-1 [&_h2]:text-md [&_h2]:font-semibold',
  '[&_h3]:mt-2 [&_h3]:mb-1 [&_h3]:font-semibold [&_p]:my-1.5 [&_ul]:my-1.5 [&_ul]:list-disc [&_ul]:pl-5 [&_ol]:my-1.5 [&_ol]:list-decimal [&_ol]:pl-5',
  '[&_li]:my-0.5 [&_a]:text-primary [&_a]:underline [&_a]:underline-offset-2 [&_hr]:my-3 [&_hr]:border-border',
  '[&_code]:rounded-sm [&_code]:bg-muted [&_code]:px-1 [&_code]:font-mono [&_code]:text-sm',
  '[&_pre]:my-2 [&_pre]:overflow-x-auto [&_pre]:rounded-md [&_pre]:bg-muted [&_pre]:p-2 [&_pre_code]:bg-transparent [&_pre_code]:p-0',
  '[&_blockquote]:my-2 [&_blockquote]:border-l-2 [&_blockquote]:pl-3 [&_blockquote]:text-muted-foreground',
  '[&_table]:my-2 [&_table]:w-full [&_table]:border-collapse [&_td]:border [&_td]:px-2 [&_td]:py-1 [&_th]:border [&_th]:bg-muted/50 [&_th]:px-2 [&_th]:py-1 [&_th]:text-left',
  '[&_input[type=checkbox]]:mr-1.5',
)

export default function NotesPreview({ text, className }: { text: string; className?: string }) {
  if (!text.trim()) return <p className={cn('text-sm text-muted-foreground', className)}>Nothing to preview.</p>
  return (
    <div className={cn(markdownClass, className)}>
      <Markdown
        remarkPlugins={[remarkGfm]}
        components={{
          a: ({ node: _node, ...props }) => <a {...props} target="_blank" rel="noopener noreferrer nofollow" />,
          img: ({ node: _node, alt }) => <span className="text-muted-foreground">[image{alt ? `: ${alt}` : ''}]</span>,
        }}
      >
        {text}
      </Markdown>
    </div>
  )
}
