import { expect, type Page, type TestInfo } from "@playwright/test"

export const PAGES = [
  { path: "/", heading: "仪表盘", slug: "dashboard" },
  { path: "/jobs", heading: "扫描任务", slug: "jobs" },
  { path: "/results", heading: "扫描结果", slug: "results" },
  { path: "/proxies", heading: "出站代理", slug: "proxies" },
  { path: "/logs", heading: "运行日志", slug: "logs" },
  { path: "/settings", heading: "设置", slug: "settings" },
] as const

/** Collects console errors and uncaught exceptions so a test can assert the page was clean. */
export function watchErrors(page: Page): string[] {
  const errors: string[] = []
  page.on("pageerror", (e) => errors.push(`pageerror: ${e.message}`))
  page.on("console", (m) => {
    if (m.type() !== "error") return
    const text = m.text()
    // a 401/abort on a request cancelled by navigation is not an app fault
    if (/Failed to load resource|net::ERR_ABORTED|NS_BINDING_ABORTED/.test(text)) return
    errors.push(`console.error: ${text}`)
  })
  return errors
}

/** The page itself must never scroll sideways (wide tables scroll inside their own container). */
export async function expectNoPageOverflow(page: Page) {
  const m = await page.evaluate(() => ({
    scroll: document.documentElement.scrollWidth,
    client: document.documentElement.clientWidth,
    body: document.body.scrollWidth,
  }))
  expect(m.scroll, `document scrollWidth ${m.scroll} > viewport ${m.client}`).toBeLessThanOrEqual(m.client)
  expect(m.body, `body scrollWidth ${m.body} > viewport ${m.client}`).toBeLessThanOrEqual(m.client)
}

/**
 * Visible controls and headings that stick out past the viewport edge, ignoring anything inside a
 * horizontally scrollable container (a table that scrolls is fine; a button you cannot reach is not).
 */
export async function offscreenControls(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const vw = document.documentElement.clientWidth
    const out: string[] = []
    const scrollable = (el: Element | null) => {
      for (let n = el; n && n !== document.body; n = n.parentElement) {
        const s = getComputedStyle(n)
        if (/(auto|scroll)/.test(s.overflowX) && n.scrollWidth > n.clientWidth) return true
      }
      return false
    }
    const sel = "button, a[href], input, textarea, [role=combobox], [role=tab], [role=switch], [role=checkbox], h1"
    for (const el of document.querySelectorAll(sel)) {
      const r = el.getBoundingClientRect()
      if (r.width === 0 || r.height === 0) continue
      const cs = getComputedStyle(el)
      if (cs.visibility === "hidden" || cs.display === "none") continue
      if (el.closest(".sr-only") || el.matches(".sr-only")) continue
      if (r.left < -1 || r.right > vw + 1) {
        if (scrollable(el.parentElement)) continue
        const label = (el.getAttribute("aria-label") || el.textContent || el.tagName).trim().slice(0, 40)
        out.push(`${el.tagName.toLowerCase()} "${label}" x=${Math.round(r.left)}..${Math.round(r.right)} (viewport ${vw})`)
      }
    }
    return out
  })
}

/** Touch targets smaller than 24x24 CSS px (WCAG 2.2 AA minimum) among visible controls. */
export async function tinyTargets(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const out: string[] = []
    for (const el of document.querySelectorAll("button, a[href], [role=switch], [role=checkbox], [role=tab]")) {
      const r = el.getBoundingClientRect()
      if (r.width === 0 || r.height === 0) continue
      if (el.closest(".sr-only")) continue
      // the switch widens its hit area with an ::after pseudo-element, which cannot be measured
      if (el.getAttribute("data-slot") === "switch") continue
      // a checkbox next to its <label> is toggled by tapping the whole label row
      if (el.getAttribute("role") === "checkbox") {
        const lab = el.parentElement?.querySelector("label")
        if (lab && lab.getBoundingClientRect().height >= 24) continue
      }
      // inline links inside running text are exempt from the minimum
      if (el.tagName === "A" && getComputedStyle(el).display === "inline") continue
      if (r.width < 24 || r.height < 24) {
        const label = (el.getAttribute("aria-label") || el.textContent || el.tagName).trim().slice(0, 40)
        out.push(`${el.tagName.toLowerCase()} "${label}" ${Math.round(r.width)}x${Math.round(r.height)}`)
      }
    }
    return out
  })
}

export async function isMobileProfile(page: Page): Promise<boolean> {
  return (page.viewportSize()?.width ?? 1024) < 768
}

/** Opens the navigation (a sheet on narrow screens, a sidebar on wide ones) and follows a link. */
export async function navigateVia(page: Page, label: string) {
  const link = page.getByRole("link", { name: label, exact: true }).first()
  if ((page.viewportSize()?.width ?? 1024) < 768) {
    // the drawer from the previous step must be fully closed before it is opened again
    await expect(link).toBeHidden()
    await page.getByRole("button", { name: "Toggle Sidebar" }).first().click()
  }
  await link.click()
}

export async function shot(page: Page, info: TestInfo, name: string) {
  await page.screenshot({ path: `e2e-screenshots/${info.project.name}/${name}.png`, fullPage: false })
}

/** Names are unique per device project, so parallel projects sharing one stack never touch each other. */
export const tag = (project: string, what: string) => `e2e-${project}-${what}`

/** Remove proxies this project created, even if a test failed half way. */
export async function cleanupTestProxies(page: Page, prefix: string) {
  const res = await page.request.get("/api/outbounds")
  if (!res.ok()) return
  const body = (await res.json()) as { items: { id: number; name: string }[] }
  for (const o of body.items) {
    if (o.name.startsWith(prefix)) await page.request.delete(`/api/outbounds/${o.id}`)
  }
}

export async function cleanupTestJobs(page: Page, prefix: string) {
  const res = await page.request.get("/api/jobs")
  if (!res.ok()) return
  const body = (await res.json()) as { items: { id: number; name: string }[] }
  for (const j of body.items) {
    if (j.name.startsWith(prefix)) {
      await page.request.post(`/api/jobs/${j.id}/cancel`).catch(() => {})
      await page.request.delete(`/api/jobs/${j.id}`)
    }
  }
}
