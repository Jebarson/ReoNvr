import assert from 'node:assert/strict'
import { mkdir, writeFile } from 'node:fs/promises'
import { dirname } from 'node:path'

export const liveCameraScreenshotSelector = 'input:not([type=checkbox]):not([type=radio]), select, img, video'

export async function captureMaskedScreenshot(page, options) {
  await page.evaluate(async () => { await document.fonts.ready; window.scrollTo(0, 0) })
  const areas = (await Promise.all(options.mask.map((locator) => locator.evaluateAll((elements) => elements.map((element) => {
    const bounds = element.getBoundingClientRect()
    return { x: bounds.x + scrollX, y: bounds.y + scrollY, width: bounds.width, height: bounds.height }
  }).filter((bounds) => bounds.width > 0 && bounds.height > 0))))).flat()
  const { path, ...captureOptions } = options
  const image = await page.screenshot({ ...captureOptions, maskColor: '#202020' })
  const opaque = await page.evaluate(async ({ screenshot, regions }) => {
    const bitmap = new Image()
    bitmap.src = `data:image/png;base64,${screenshot}`
    await bitmap.decode()
    const canvas = document.createElement('canvas')
    canvas.width = bitmap.width
    canvas.height = bitmap.height
    const context = canvas.getContext('2d')
    context.drawImage(bitmap, 0, 0)
    return regions.every((area) => {
      const pixel = context.getImageData(Math.floor((area.x + area.width / 2) * devicePixelRatio), Math.floor((area.y + area.height / 2) * devicePixelRatio), 1, 1).data
      return pixel[0] === 32 && pixel[1] === 32 && pixel[2] === 32 && pixel[3] === 255
    })
  }, { screenshot: image.toString('base64'), regions: areas })
  assert.equal(opaque, true, 'Screenshot redactions must be solid opaque pixels')
  if (path) {
    await mkdir(dirname(path), { recursive: true })
    await writeFile(path, image)
  }
  return image
}