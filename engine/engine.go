package engine

import (
	"code.google.com/p/mx3/cuda"
	"code.google.com/p/mx3/data"
	"code.google.com/p/mx3/util"
	cuda5 "github.com/barnex/cuda5/cuda"
	"log"
	"time"
)

// User inputs
var (
	Aex     ScalFn        = Const(0)             // Exchange stiffness in J/m
	ExMask  StaggeredMask                        // Mask that scales Aex/Msat between cells.
	Msat    ScalFn        = Const(0)             // Saturation magnetization in A/m
	Alpha   ScalFn        = Const(0)             // Damping constant
	B_ext   VecFn         = ConstVector(0, 0, 0) // External field in T
	DMI     ScalFn        = Const(0)             // Dzyaloshinskii-Moriya vector in J/m²
	Ku1     VecFn         = ConstVector(0, 0, 0) // Uniaxial anisotropy vector in J/m³
	Xi      ScalFn        = Const(0)             // Non-adiabaticity of spin-transfer-torque
	SpinPol ScalFn        = Const(1)             // Spin polarization of electrical current
	J       VecFn         = ConstVector(0, 0, 0) // Electrical current density
)

func init() { ExMask = StaggeredMask{buffered: buffered{autosave: autosave{name: "exmask"}}} }

// Accessible quantities
var (
	M      *buffered // reduced magnetization (unit length)
	AvgM   *scalar   // average magnetization
	Torque *setter   // torque (?) output handle

	B_eff   *setter // effective field (T) output handle
	B_demag *setter // demag field (T) output handle
	B_dmi   *adder  // demag field (T) output handle
	B_exch  *adder  // exchange field (T) output handle
	B_uni   *adder  // field due to uniaxial anisotropy output handle
	STT     *adder  // spin-transfer torque output handle

	Table  *DataTable // output handle for tabular data (average magnetization etc.)
	Time   float64    // time in seconds  // todo: hide? setting breaks autosaves
	Solver *cuda.Heun
)

// hidden quantities
var (
	mesh         data.Mesh
	torquebuffer *data.Slice
	vol          *data.Slice
	postStep     []func() // called on after every time step
	extFields    []extField
	itime        int //unique integer time stamp
)

// Add an additional space-dependent field to B_ext.
// The field is mask * multiplier, where mask typically contains space-dependent scaling values of the order of 1.
// multiplier can be time dependent.
// TODO: extend API (set one component, construct masks or read from file). Also for current.
func AddExtField(mask *data.Slice, multiplier ScalFn) {
	m := cuda.GPUCopy(mask)
	extFields = append(extFields, extField{m, multiplier})
}

type extField struct {
	mask *data.Slice
	mul  ScalFn
}

func initialize() {

	// these 2 GPU arrays are re-used to stored various quantities.
	torquebuffer = cuda.NewSlice(3, &mesh)

	// cell volumes currently unused
	vol = data.NilSlice(1, &mesh)

	// magnetization
	M = newBuffered(cuda.NewSlice(3, &mesh), "m")
	quants["m"] = M
	AvgM = newScalar(3, "m", "", func() []float64 {
		return M.Average()
	})

	// data table
	Table = newTable("datatable")
	Table.Add(AvgM)

	// demag field
	demag_ := cuda.NewDemag(&mesh)
	B_demag = newSetter(3, &mesh, "B_demag", func(b *data.Slice) {
		demag_.Exec(b, M.buffer, vol, Mu0*Msat()) //TODO: consistent msat or bsat
	})

	// exchange field
	B_exch = newAdder(3, &mesh, "B_exch", func(dst *data.Slice) {
		cuda.AddExchange(dst, M.buffer, ExMask.buffer, Aex(), Msat())
	})

	// Dzyaloshinskii-Moriya field
	B_dmi = newAdder(3, &mesh, "B_dmi", func(dst *data.Slice) {
		d := DMI()
		if d != 0 {
			cuda.AddDMI(dst, M.buffer, d, Msat())
		}
	})

	// uniaxial anisotropy
	B_uni = newAdder(3, &mesh, "B_uni", func(dst *data.Slice) {
		ku1 := Ku1() // in J/m3
		if ku1 != [3]float64{0, 0, 0} {
			cuda.AddUniaxialAnisotropy(dst, M.buffer, ku1[2], ku1[1], ku1[0], Msat())
		}
	})

	// external field
	B_ext = newAdder(3, &mesh, "B_ext", func(dst *data.Slice) {
		bext := B_ext()
		cuda.AddConst(dst, float32(bext[2]), float32(bext[1]), float32(bext[0]))
		for _, f := range extFields {
			cuda.Madd2(dst, dst, f.mask, 1, float32(f.mul()))
		}
	})

	// effective field
	B_eff = newSetter(3, &mesh, "B_eff", func(dst *data.Slice) {
		B_demag.set(dst)
		B_exch.addTo(dst)
		B_dmi.addTo(dst)
		B_uni.addTo(dst)
		B_ext.addTo(dst)
	})
	quants["B_eff"] = b_eff

	// llg torque
	Torque = newSetter(3, &mesh, "torque", func(b *data.Slice) {
		cuda.LLGTorque(b, M.buffer, b, float32(Alpha()))
	})
	quants["torque"] = torque

	// spin-transfer torque
	STT = newAdder(3, &mesh, "stt", func(dst *data.Slice) {
		j := J()
		if j != [3]float64{0, 0, 0} {
			p := SpinPol()
			jx := j[2] * p
			jy := j[1] * p
			jz := j[0] * p
			cuda.AddZhangLiTorque(dst, M.buffer, [3]float64{jx, jy, jz}, Msat(), nil, Alpha(), Xi())
		}
	})

	// solver
	torqueFn := func(good bool) *data.Slice {
		itime++
		Table.arm() // if table output needed, quantities marked for update
		M.touch()   // saves m if needed
		ExMask.touch()
		B_eff.set(torquebuffer)
		Torque.set(torquebuffer)
		stt.addTo(torquebuffer)
		Table.touch() // all needed quantities are now up-to-date, save them
		return torquebuffer
	}
	Solver = cuda.NewHeun(M.buffer, torqueFn, cuda.Normalize, 1e-15, Gamma0, &Time)
}

// Register function f to be called after every time step.
// Typically used, e.g., to manipulate the magnetization.
func PostStep(f func()) {
	postStep = append(postStep, f)
}

func step() {
	Solver.Step()
	for _, f := range postStep {
		f()
	}
	s := Solver
	util.Dashf("step: % 8d (%6d) t: % 12es Δt: % 12es ε:% 12e", s.NSteps, s.NUndone, Time, s.Dt_si, s.LastErr)
}

// injects arbitrary code into the engine run loops. Used by web interface.
var inject = make(chan func()) // inject function calls into the cuda main loop. Executed in between time steps.

// inject code into engine and wait for it to complete.
func injectAndWait(task func()) {
	ready := make(chan int)
	inject <- func() { task(); ready <- 1 }
	<-ready
}

// Returns the mesh cell size in meters. E.g.:
// 	cellsize_x := CellSize()[X]
func CellSize() [3]float64 {
	c := mesh.CellSize()
	return [3]float64{c[Z], c[Y], c[X]} // swaps XYZ
}

func WorldSize() [3]float64 {
	w := mesh.WorldSize()
	return [3]float64{w[Z], w[Y], w[X]} // swaps XYZ
}

func GridSize() [3]int {
	n := mesh.Size()
	return [3]int{n[Z], n[Y], n[X]} // swaps XYZ
}

// Run the simulation for a number of seconds.
func Run(seconds float64) {
	log.Println("run for", seconds, "s")
	stop := Time + seconds
	RunCond(func() bool { return Time < stop })
}

// Run the simulation for a number of steps.
func Steps(n int) {
	log.Println("run for", n, "steps")
	stop := Solver.NSteps + n
	RunCond(func() bool { return Solver.NSteps < stop })
}

// Runs as long as condition returns true.
func RunCond(condition func() bool) {
	checkInited() // todo: check in handler
	defer util.DashExit()

	pause = false
	for condition() && !pause {
		select {
		default:
			step()
		case f := <-inject:
			f()
		}
	}
	pause = true
}

// Enter interactive mode. Simulation is now exclusively controlled
// by web GUI (default: http://localhost:35367)
func RunInteractive() {
	lastKeepalive = time.Now()
	pause = true
	log.Println("entering interactive mode")
	if webPort == "" {
		goServe(*flag_port)
	}

	for {
		if time.Since(lastKeepalive) > webtimeout {
			log.Println("interactive session idle: exiting")
			break
		}
		log.Println("awaiting interaction")
		f := <-inject
		f()
	}
}

// Set the simulation mesh to Nx x Ny x Nz cells of given size.
// Can be set only once at the beginning of the simulation.
func SetMesh(Nx, Ny, Nz int, cellSizeX, cellSizeY, cellSizeZ float64) {
	var zeromesh data.Mesh
	if mesh != zeromesh {
		free()
	}
	if Nx <= 1 {
		log.Fatal("mesh size X should be > 1, have: ", Nx)
	}
	mesh = *data.NewMesh(Nz, Ny, Nx, cellSizeZ, cellSizeY, cellSizeX)
	log.Println("set mesh:", mesh.UserString())
	initialize()
}

// TODO: not perfectly OK yet.
func free() {
	log.Println("resetting gpu")
	cuda5.DeviceReset() // does not seem to clear allocations
	Init()
	dlQue = nil
}

func checkInited() {
	if mesh.Size() == [3]int{0, 0, 0} {
		log.Fatal("need to set mesh first")
	}
}
