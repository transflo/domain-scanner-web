import { expect, test, type Page } from "@playwright/test"

import {
  cleanupTestJobs,
  cleanupTestProxies,
  tag,
  expectNoPageOverflow,
  offscreenControls,
  shot,
  watchErrors,
} from "./helpers"

const UUID = "b831381d-6324-4d53-ad4f-8cda48b30811"
// .invalid never resolves, so nothing here can reach a real server
const link = (name: string) => `vless://${UUID}@e2e-import.invalid:443?encryption=none&security=tls&sni=e2e-import.invalid&type=ws&path=%2Fe2e#${name}`

async function pick(page: Page, label: string, option: string | RegExp) {
  await page.getByRole("combobox", { name: label }).click()
  await page.getByRole("option", { name: option }).click()
}

/** The dialog/sheet must sit inside the screen, with its buttons reachable. */
async function expectFitsViewport(page: Page, testId: string) {
  const box = await page.getByTestId(testId).boundingBox()
  const vp = page.viewportSize()!
  expect(box, `${testId} is not rendered`).not.toBeNull()
  expect(box!.x).toBeGreaterThanOrEqual(-1)
  expect(box!.x + box!.width).toBeLessThanOrEqual(vp.width + 1)
  expect(box!.y).toBeGreaterThanOrEqual(-1)
  expect(box!.y + box!.height).toBeLessThanOrEqual(vp.height + 1)
}

test.describe("outbound proxies", () => {
  test.afterEach(async ({ page }, info) => {
    await cleanupTestProxies(page, tag(info.project.name, ""))
  })

  test("add by form, by JSON round trip, and by import; test, edit, disable, delete", async ({ page }, info) => {
    const basic = tag(info.project.name, "basic")
    const imp = tag(info.project.name, "import")
    const errors = watchErrors(page)
    await page.goto("/proxies")
    await expect(page.getByRole("heading", { name: "出站代理", level: 1 })).toBeVisible()

    // ---- basics tab
    await page.getByRole("button", { name: "添加" }).click()
    const dialog = page.getByTestId("outbound-dialog")
    await expect(dialog).toBeVisible()
    await expectFitsViewport(page, "outbound-dialog")
    await shot(page, info, "proxy-dialog-basics")

    await dialog.getByLabel("备注名称").first().fill(basic)
    await dialog.getByLabel("服务器地址").fill("e2e-basic.invalid")
    await dialog.getByRole("button", { name: "测活" }).click()
    await expect(dialog.getByRole("alert")).toContainText("UUID") // incomplete form is refused before any request
    await dialog.getByLabel("UUID").fill(UUID)

    // ---- JSON tab shows what the form builds, and the form keeps its values on the way back
    await dialog.getByRole("tab", { name: "JSON" }).click()
    const json = dialog.getByLabel("Xray 出站 JSON")
    await expect(json).toHaveValue(/"protocol": "vless"/)
    await expect(json).toHaveValue(/e2e-basic\.invalid/)
    await expectFitsViewport(page, "outbound-dialog")
    await dialog.getByRole("tab", { name: "基础" }).click()
    await expect(dialog.getByLabel("服务器地址")).toHaveValue("e2e-basic.invalid")

    // ---- test the unsaved config: an unreachable host must be reported as a failure, with a reason
    await dialog.getByRole("button", { name: "测活" }).click()
    await expect(dialog.getByTestId("probe-result")).toContainText("不可用", { timeout: 30_000 })
    await expect(dialog.getByTestId("probe-result")).toContainText(/\S{6,}/)

    await dialog.getByRole("button", { name: "保存" }).click()
    await expect(dialog).toBeHidden()
    const row = page.locator('[data-testid^="proxy-row-"]', { hasText: basic })
    await expect(row).toBeVisible()
    await expect(row).toContainText("vless")
    await expectNoPageOverflow(page)

    // ---- import tab: preview, then save the valid nodes only
    await page.getByRole("button", { name: "添加" }).click()
    await dialog.getByRole("tab", { name: "导入" }).click()
    await dialog.getByLabel("分享链接 / 订阅内容 / JSON").fill(`${link(imp)}\nnot-a-link`)
    await dialog.getByRole("button", { name: "解析预览" }).click()
    const preview = dialog.getByTestId("import-preview")
    await expect(preview).toContainText("解析出 1 个节点")
    await expect(preview).toContainText("1 行无法解析")
    await shot(page, info, "proxy-dialog-import")
    await expectFitsViewport(page, "outbound-dialog")
    await dialog.getByRole("button", { name: "导入全部有效节点" }).click()
    await expect(dialog).toBeHidden()
    const imported = page.locator('[data-testid^="proxy-row-"]', { hasText: imp })
    await expect(imported).toBeVisible()
    await expect(imported).toContainText("ws")
    await expect(imported).toContainText("tls")

    // ---- liveness test from the list
    await imported.getByRole("button", { name: "测活" }).click()
    await expect(imported.locator('[data-testid^="proxy-result-"]')).toBeVisible({ timeout: 40_000 })
    await shot(page, info, "proxies-list")
    await expectNoPageOverflow(page)
    expect(await offscreenControls(page)).toEqual([])

    // ---- edit opens on the basics tab, prefilled, because the form can represent this config
    await imported.getByRole("button", { name: /编辑/ }).click()
    await expect(dialog.getByLabel("服务器地址")).toHaveValue("e2e-import.invalid")
    await expect(dialog.getByLabel("路径")).toHaveValue("/e2e")
    await dialog.getByRole("button", { name: "取消" }).click()
    await expect(dialog).toBeHidden()

    // ---- disable
    await imported.getByRole("switch").click()
    await expect(imported).toContainText("已停用")

    // ---- delete both through the confirmation dialog
    for (const name of [imp, basic]) {
      const r = page.locator('[data-testid^="proxy-row-"]', { hasText: name })
      await r.getByRole("button", { name: /删除/ }).click()
      await page.getByRole("alertdialog").getByRole("button", { name: "删除" }).click()
      await expect(r).toHaveCount(0)
    }
    expect(errors).toEqual([])
  })

  test("a config the form cannot represent opens on the JSON tab", async ({ page }, info) => {
    const custom = tag(info.project.name, "custom")
    const res = await page.request.post("/api/outbounds", {
      data: {
        name: custom,
        enabled: false,
        config: JSON.stringify({
          protocol: "vless",
          settings: { address: "e2e-custom.invalid", port: 443, id: UUID, encryption: "none" },
          streamSettings: { network: "tcp", security: "none", sockopt: { mark: 255 } },
        }),
      },
    })
    expect(res.ok()).toBeTruthy()
    await page.goto("/proxies")
    const row = page.locator('[data-testid^="proxy-row-"]', { hasText: custom })
    await row.getByRole("button", { name: /编辑/ }).click()
    const dialog = page.getByTestId("outbound-dialog")
    await expect(dialog.getByLabel("Xray 出站 JSON")).toHaveValue(/sockopt/)
    // switching to the form would drop sockopt, so it must refuse
    await dialog.getByRole("tab", { name: "基础" }).click()
    await expect(dialog.getByLabel("Xray 出站 JSON")).toBeVisible()
  })
})

test.describe("jobs", () => {
  test.afterEach(async ({ page }, info) => {
    await cleanupTestJobs(page, tag(info.project.name, ""))
  })

  test("the new-job sheet fits the screen and offers an egress choice", async ({ page }, info) => {
    await page.goto("/jobs")
    await page.getByRole("button", { name: /新建任务/ }).click()
    const section = page.getByTestId("egress-section")
    await section.scrollIntoViewIfNeeded()
    await expect(section).toBeVisible()
    await expect(section).toContainText("直连")

    // choosing a specific proxy asks for one and blocks submission until it is chosen
    await pick(page, "出站方式", /指定代理/)
    await expect(section.getByRole("combobox", { name: "代理" })).toBeVisible()
    await expect(page.getByRole("button", { name: "创建并开始" })).toBeDisabled()
    await pick(page, "出站方式", /代理池/)
    await expect(section.getByRole("combobox", { name: "代理" })).toHaveCount(0)
    await expect(page.getByRole("button", { name: "创建并开始" })).toBeEnabled()
    await shot(page, info, "job-form-egress")

    const submit = page.getByRole("button", { name: "创建并开始" })
    await submit.scrollIntoViewIfNeeded()
    const box = await submit.boundingBox()
    const vp = page.viewportSize()!
    expect(box!.x + box!.width).toBeLessThanOrEqual(vp.width + 1)
    expect(box!.y + box!.height).toBeLessThanOrEqual(vp.height + 1)
    await expectNoPageOverflow(page)
  })

  test("a job can be created with an explicit direct egress and shows it", async ({ page }, info) => {
    const name = tag(info.project.name, "job")
    await page.goto("/jobs")
    await page.getByRole("button", { name: /新建任务/ }).click()
    await page.getByLabel("名称(可选)").fill(name)
    await page.getByLabel("域名后缀").fill(".test")
    await page.getByLabel("长度").fill("1")
    await page.getByRole("button", { name: "创建并开始" }).click()
    const row = page.locator('[data-testid^="job-row-"]', { hasText: name })
    await expect(row).toBeVisible()
    await expect(row).toContainText("直连")
    await expectNoPageOverflow(page)
  })
})

test.describe("logs", () => {
  test("lines expand into structured detail; diagnostics and export are available", async ({ page }, info) => {
    const errors = watchErrors(page)
    await page.goto("/logs")
    const lines = page.getByTestId("log-line")
    await expect(lines.first()).toBeVisible()
    await lines.first().getByRole("button").click()
    const detail = page.getByTestId("log-detail").first()
    await expect(detail).toContainText("组件 / 事件")
    await expectNoPageOverflow(page)
    await shot(page, info, "logs-expanded")

    // filters narrow the stream on the server side
    await page.getByLabel("关键字").fill("zzz-no-such-text-zzz")
    await expect(lines).toHaveCount(0)
    await expect(page.getByTestId("log-box")).toContainText("暂无日志")
    await page.getByLabel("关键字").fill("")
    await expect(lines.first()).toBeVisible()

    // the export menu offers the whole server-side history, not just what this page has loaded
    await page.getByRole("button", { name: /导出/ }).click()
    const all = page.getByRole("menuitem", { name: /全部日志 · 文本/ })
    await expect(all).toHaveAttribute("href", /\/api\/logs\/export\?format=text$/)
    await expect(page.getByRole("menuitem", { name: /全部日志 · JSONL/ })).toHaveAttribute("href", "/api/logs/export")
    const box = await page.getByRole("menu").boundingBox()
    const vp = page.viewportSize()!
    expect(box!.x).toBeGreaterThanOrEqual(-1)
    expect(box!.x + box!.width).toBeLessThanOrEqual(vp.width + 1)
    await shot(page, info, "logs-export-menu")
    await page.keyboard.press("Escape")

    await page.getByRole("tab", { name: "诊断统计" }).click()
    await expect(page.getByTestId("diag-summary")).toBeVisible()
    await expectNoPageOverflow(page)
    await shot(page, info, "logs-diagnostics")
    expect(errors).toEqual([])
  })
})

test.describe("settings", () => {
  test("shows the new sections and never exposes a full secret", async ({ page }, info) => {
    await page.goto("/settings")
    for (const title of ["Telegram 通知", "Cloudflare Registrar", "一键注册策略", "日志与代理", "访问保护"]) {
      await expect(page.getByText(title, { exact: true }).first()).toBeVisible()
    }
    await expect(page.getByTestId("cloudflare-state")).toBeVisible()
    await expect(page.getByRole("checkbox", { name: /注册前二次确认/ })).toBeVisible()
    await expect(page.getByText("存储与保留", { exact: true })).toBeVisible()
    await expect(page.getByTestId("storage-state")).toBeVisible()
    await expect(page.getByTestId("storage-db")).toContainText(/\d/)
    await expect(page.getByRole("button", { name: "立即清理" })).toBeVisible()
    await shot(page, info, "settings")
    await expectNoPageOverflow(page)

    // saved secrets come back masked
    const cfToken = await page.getByLabel("API Token").inputValue()
    if (cfToken) expect(cfToken).toContain("****")
    const tgToken = await page.getByLabel("Bot Token").inputValue()
    if (tgToken) expect(tgToken).toContain("****")
    const html = await page.content()
    expect(html).not.toMatch(/\d{8,}:[A-Za-z0-9_-]{30,}/)
    expect(html).not.toMatch(/cfat_[A-Za-z0-9]{20,}/)
  })
})

test.describe("results", () => {
  test("has the Cloudflare filter and column", async ({ page }, info) => {
    await page.goto("/results")
    await expect(page.getByRole("combobox", { name: "Cloudflare 筛选" })).toBeVisible()
    await pick(page, "状态筛选", /全部/)
    await expect(page.getByTestId("results-total")).toBeVisible()
    const rows = page.locator("tbody tr")
    if ((await rows.count()) > 0) {
      await expect(page.getByRole("columnheader", { name: "Cloudflare" })).toBeVisible()
    }
    await expectNoPageOverflow(page)
    await shot(page, info, "results-all")
  })
})
