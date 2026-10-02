"use client"

import { Label } from "@/components/ui/label"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { cn } from "@/lib/utils"

/** A label above a control, with an optional hint underneath. */
export function Field({
  label,
  htmlFor,
  hint,
  className,
  children,
}: {
  label: string
  htmlFor?: string
  hint?: React.ReactNode
  className?: string
  children: React.ReactNode
}) {
  return (
    <div className={cn("flex min-w-0 flex-col gap-2", className)}>
      <Label htmlFor={htmlFor}>{label}</Label>
      {children}
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
    </div>
  )
}

export interface Option {
  value: string
  label: string
}

/** A labelled single-select over `{value,label}` options. */
export function SelectField({
  label,
  value,
  onChange,
  options,
  hint,
  className,
  placeholder,
}: {
  label: string
  value: string
  onChange: (v: string) => void
  options: Option[]
  hint?: React.ReactNode
  className?: string
  placeholder?: string
}) {
  return (
    <Field label={label} hint={hint} className={className}>
      <Select value={value} onValueChange={(v) => onChange(String(v))} items={options}>
        <SelectTrigger className="w-full min-w-0" aria-label={label}>
          <SelectValue placeholder={placeholder} />
        </SelectTrigger>
        <SelectContent>
          {options.map((o) => (
            <SelectItem key={o.value} value={o.value}>
              {o.label}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </Field>
  )
}
