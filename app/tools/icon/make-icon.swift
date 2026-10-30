import AppKit

let size: CGFloat = 1024
let out = CommandLine.arguments.count > 1 ? CommandLine.arguments[1] : "icon.png"

let cs = CGColorSpace(name: CGColorSpace.sRGB)!
let ctx = CGContext(data: nil, width: Int(size), height: Int(size), bitsPerComponent: 8, bytesPerRow: 0,
                    space: cs, bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)!

func rgb(_ r: CGFloat, _ g: CGFloat, _ b: CGFloat, _ a: CGFloat = 1) -> CGColor {
    CGColor(colorSpace: cs, components: [r / 255, g / 255, b / 255, a])!
}

let inset: CGFloat = 100
let tile = CGRect(x: inset, y: inset, width: size - 2 * inset, height: size - 2 * inset)
let tilePath = CGPath(roundedRect: tile, cornerWidth: 185, cornerHeight: 185, transform: nil)

ctx.saveGState()
ctx.setShadow(offset: CGSize(width: 0, height: -12), blur: 28, color: rgb(0, 0, 0, 0.35))
ctx.addPath(tilePath)
ctx.setFillColor(rgb(20, 70, 150))
ctx.fillPath()
ctx.restoreGState()

ctx.saveGState()
ctx.addPath(tilePath)
ctx.clip()
let ground = CGGradient(colorsSpace: cs, colors: [rgb(38, 198, 170), rgb(20, 70, 150)] as CFArray, locations: [0, 1])!
ctx.drawLinearGradient(ground, start: CGPoint(x: tile.minX, y: tile.maxY), end: CGPoint(x: tile.maxX, y: tile.minY), options: [])
ctx.restoreGState()

func slab(_ r: CGRect, _ color: CGColor) {
    ctx.saveGState()
    ctx.setShadow(offset: CGSize(width: 0, height: -10), blur: 22, color: rgb(0, 0, 0, 0.30))
    ctx.addPath(CGPath(roundedRect: r, cornerWidth: 36, cornerHeight: 36, transform: nil))
    ctx.setFillColor(color)
    ctx.fillPath()
    ctx.restoreGState()
}
let x0: CGFloat = 330, bar: CGFloat = 120
slab(CGRect(x: x0, y: 250, width: bar, height: 520), rgb(255, 255, 255))
slab(CGRect(x: x0, y: 650, width: 380, height: bar), rgb(255, 255, 255))
slab(CGRect(x: x0, y: 470, width: 290, height: bar), rgb(214, 244, 238))

ctx.saveGState()
ctx.addPath(tilePath)
ctx.clip()
let shine = CGGradient(colorsSpace: cs, colors: [rgb(255, 255, 255, 0.16), rgb(255, 255, 255, 0)] as CFArray,
                       locations: [0, 1])!
ctx.drawLinearGradient(shine, start: CGPoint(x: 0, y: tile.maxY), end: CGPoint(x: 0, y: tile.midY + 100), options: [])
ctx.restoreGState()

let image = ctx.makeImage()!
let rep = NSBitmapImageRep(cgImage: image)
try! rep.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: out))
