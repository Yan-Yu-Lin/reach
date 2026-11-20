import { chromium } from '/Users/linyanyu/20-29-Development/21-Active-Projects/instagram-chat-export/node_modules/playwright-core/index.mjs'
const b64 = (o) => Buffer.from(JSON.stringify(o)).toString('base64url')
const FAKE = `${b64({ alg: 'none' })}.${b64({ sub: 'audit', exp: 4102444800 })}.x`
const browser = await chromium.launch({ executablePath: process.env.CHROME })
const ctx = await browser.newContext({ viewport: { width: 393, height: 852 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true, colorScheme: 'dark' })
await ctx.addInitScript((t) => localStorage.setItem('reach-token', t), FAKE)
const page = await ctx.newPage(); await page.goto('http://127.0.0.1:9388/'); await page.waitForSelector('.fleet-item')
await page.addStyleTag({ content: '@media (max-width:768px){.stage{grid-row:3}.mobile-tabs{grid-row:4}.app-shell{height:100dvh}}' })
await page.waitForTimeout(400)
await page.screenshot({ path: process.env.SHOTS + '/20-mobile-with-2-line-css-fix.png' })
await page.click('.fleet-item >> nth=1'); await page.waitForTimeout(300)
await page.screenshot({ path: process.env.SHOTS + '/21-mobile-detail-with-css-fix.png' }); await page.click('.mobile-tab >> nth=0'); await page.waitForTimeout(300); await page.evaluate(() => document.querySelector('.stage').scrollTo(0, 700)); await page.waitForTimeout(200); await page.screenshot({ path: process.env.SHOTS + '/22-mobile-inbox-scrolled-with-css-fix.png' })
await browser.close()
