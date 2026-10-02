import { expect, test } from "@playwright/test"

import {
  expectNoPageOverflow,
  isMobileProfile,
  navigateVia,
  offscreenControls,
  PAGES,
  shot,
  tinyTargets,
  watchErrors,
} from "./helpers"

// Every page, on every device profile: it renders, nothing sticks out sideways, no console errors.
for (const p of PAGES) {
  test(`${p.slug}: renders without overflow or errors`, async ({ page }, info) => {
    const errors = watchErrors(page)
    await page.goto(p.path)
    await expect(page.getByRole("heading", { name: p.heading, level: 1 })).toBeVisible()
    // let the polled data arrive so tables/cards are laid out with real content
    await page.waitForLoadState("networkidle")
    await expectNoPageOverflow(page)
    expect(await offscreenControls(page), "controls outside the viewport").toEqual([])
    await shot(page, info, p.slug)
    expect(errors).toEqual([])
  })
}

test("navigation reaches every page", async ({ page }) => {
  await page.goto("/")
  await expect(page.getByRole("heading", { name: "仪表盘", level: 1 })).toBeVisible()
  for (const p of PAGES.slice(1)) {
    await navigateVia(page, p.heading === "运行日志" ? "运行日志" : p.heading === "设置" ? "设置" : p.heading)
    await expect(page).toHaveURL(new RegExp(`${p.path}$`))
    await expect(page.getByRole("heading", { name: p.heading, level: 1 })).toBeVisible()
  }
})

test("the sidebar is a drawer on phones and a fixed column on wide screens", async ({ page }) => {
  await page.goto("/")
  const link = page.getByRole("link", { name: "出站代理", exact: true }).first()
  const width = page.viewportSize()!.width
  if (width < 768) {
    await expect(link).toBeHidden()
    await page.getByRole("button", { name: "Toggle Sidebar" }).first().click()
    await expect(link).toBeVisible()
  } else {
    await expect(link).toBeVisible()
  }
})

test("touch targets are at least 24px on touch devices", async ({ page }, info) => {
  test.skip(!info.project.use.hasTouch, "pointer devices")
  for (const p of PAGES) {
    await page.goto(p.path)
    await page.waitForLoadState("networkidle")
    const tiny = await tinyTargets(page)
    expect(tiny, `${p.path}: undersized targets`).toEqual([])
  }
})

test("dark mode renders without overflow", async ({ page }, info) => {
  await page.goto("/")
  await expect(page.getByRole("heading", { name: "仪表盘", level: 1 })).toBeVisible()
  await page.waitForLoadState("networkidle") // the key handler exists once the page has hydrated
  await page.keyboard.press("d")
  await expect(page.locator("html")).toHaveClass(/dark/)
  await expectNoPageOverflow(page)
  await shot(page, info, "dashboard-dark")
  await page.keyboard.press("d")
})

test("login page fits the screen", async ({ browser, baseURL }, info) => {
  const ctx = await browser.newContext({ ...info.project.use, storageState: { cookies: [], origins: [] }, baseURL })
  const page = await ctx.newPage()
  await page.goto("/login")
  await expect(page.getByLabel("口令")).toBeVisible()
  await expectNoPageOverflow(page)
  const box = await page.getByRole("button", { name: "登录" }).boundingBox()
  const vp = page.viewportSize()!
  expect(box!.y + box!.height).toBeLessThanOrEqual(vp.height)
  await shot(page, info, "login")
  await ctx.close()
})

test("an unauthenticated visitor is sent to the login page", async ({ browser, baseURL }, info) => {
  const ctx = await browser.newContext({ ...info.project.use, storageState: { cookies: [], origins: [] }, baseURL })
  const page = await ctx.newPage()
  await page.goto("/settings")
  await expect(page).toHaveURL(/\/login/)
  await ctx.close()
})

test("narrow screens keep the header usable", async ({ page }) => {
  test.skip(!(await isMobileProfile(page)), "wide screens")
  await page.goto("/jobs")
  await expect(page.getByRole("button", { name: /新建任务/ })).toBeVisible()
})
